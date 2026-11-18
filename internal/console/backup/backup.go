// Package backup 实现控制台的加密备份与恢复（规格书 P6-6）。
//
// 备份是逻辑备份：逐表导出（gob 编码，保留主键与二进制字段）加上数据目录中的密钥文件
// （节点 CA、私钥加密密钥、安装 ID）。因此同一份备份可以恢复到 SQLite 或 PostgreSQL，
// 也是 SQLite 迁移到 PostgreSQL 的方式。备份包含 CA 私钥，必须加密：Argon2id 派生密钥 + AES-256-GCM。
//
// 文件格式：magic(8) | salt(16) | nonce(12) | AES-GCM(tar.gz)
package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"

	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/version"
)

const magic = "ETOSBK1\n"

// 数据目录中需要随备份保存的文件（web-tls 可重新生成，不备份）。
var dataFiles = []string{"installation.json", "secret.key", "pki/ca.crt", "pki/ca.key"}

// Options 控制备份内容。
type Options struct {
	Metrics bool // 是否包含流量统计（通常很大，默认不包含）
}

// Summary 描述一份备份。
type Summary struct {
	Product   string           `json:"product"`
	Version   string           `json:"version"`
	CreatedAt time.Time        `json:"created_at"`
	Tables    map[string]int64 `json:"tables"`
	Files     []string         `json:"files"`
}

// table 把一张表的导出 / 导入绑定到具体的模型类型（gob 需要具体类型）。
type table struct {
	name   string
	model  any
	dump   func(db *gorm.DB, w io.Writer) (int64, error)
	load   func(db *gorm.DB, r io.Reader) (int64, error)
	verify func(r io.Reader) (int64, error)
}

func tbl[T any](name string) table {
	return table{
		name:  name,
		model: new(T),
		dump: func(db *gorm.DB, w io.Writer) (int64, error) {
			enc := gob.NewEncoder(w)
			var n int64
			var batch []T
			res := db.Model(new(T)).FindInBatches(&batch, 500, func(tx *gorm.DB, _ int) error {
				n += int64(len(batch))
				return enc.Encode(batch)
			})
			return n, res.Error
		},
		load: func(db *gorm.DB, r io.Reader) (int64, error) {
			dec := gob.NewDecoder(r)
			var n int64
			for {
				var batch []T
				if err := dec.Decode(&batch); err == io.EOF {
					return n, nil
				} else if err != nil {
					return n, err
				}
				if len(batch) == 0 {
					continue
				}
				// 关联字段（如 Deployment.Nodes）单独成表导入，这里不级联
				if err := db.Session(&gorm.Session{FullSaveAssociations: false}).Omit(clauseAssociations).CreateInBatches(batch, 200).Error; err != nil {
					return n, err
				}
				n += int64(len(batch))
			}
		},
		verify: func(r io.Reader) (int64, error) {
			dec := gob.NewDecoder(r)
			var n int64
			for {
				var batch []T
				if err := dec.Decode(&batch); err == io.EOF {
					return n, nil
				} else if err != nil {
					return n, err
				}
				n += int64(len(batch))
			}
		},
	}
}

const clauseAssociations = "*.*" // gorm: Omit 所有关联

// tables 按依赖顺序列出；AcmeChallenge 是进行中的临时数据，不备份（见 TestTablesCoverAllStoreModels）。
func tables() []table {
	return []table{
		tbl[store.SystemKV]("system_kvs"),
		tbl[store.User]("users"),
		tbl[store.APIToken]("api_tokens"),
		tbl[store.LicenseStore]("license_stores"),
		tbl[store.Certificate]("certificates"),
		tbl[store.AcmeOrder]("acme_orders"),
		tbl[store.AlertChannel]("alert_channels"),
		tbl[store.AlertRule]("alert_rules"),
		tbl[store.AlertEvent]("alert_events"),
		tbl[store.DataplanePackage]("dataplane_packages"),
		tbl[store.NodeGroup]("node_groups"),
		tbl[store.Node]("nodes"),
		tbl[store.NodeDrift]("node_drifts"),
		tbl[store.EnrollToken]("enroll_tokens"),
		tbl[store.StaticBundle]("static_bundles"),
		tbl[store.IPSet]("ip_sets"),
		tbl[store.Upstream]("upstreams"),
		tbl[store.StreamService]("stream_services"),
		tbl[store.Site]("sites"),
		tbl[store.Snippet]("snippets"),
		tbl[store.ConfigImport]("config_imports"),
		tbl[store.Revision]("revisions"),
		tbl[store.Deployment]("deployments"),
		tbl[store.DeploymentNode]("deployment_nodes"),
		tbl[store.ReleaseRequest]("release_requests"),
		tbl[store.CanaryRelease]("canary_releases"),
		tbl[store.DataplaneUpgrade]("dataplane_upgrades"),
		tbl[store.AuditLog]("audit_logs"),
		tbl[store.MetricMinute]("metric_minutes"),
	}
}

// 备份口令派生参数（Argon2id：3 轮、64 MiB、4 线程）。文件格式 ETOSBK1 固定使用这组参数；
// 只有本包的测试会临时调低它们（见 TestMain），生产代码没有任何开关可以改变。
var kdfTime, kdfMemoryKiB, kdfThreads uint32 = 3, 64 * 1024, 4

func deriveKey(pass, salt []byte) []byte {
	return argon2.IDKey(pass, salt, kdfTime, kdfMemoryKiB, uint8(kdfThreads), 32)
}

// Create 写出一份加密备份。
func Create(w io.Writer, st *store.Store, dataDir string, pass []byte, opt Options) (*Summary, error) {
	if len(pass) < 8 {
		return nil, errors.New("备份口令至少 8 个字符")
	}
	sum := &Summary{Product: version.Product, Version: version.Version, CreatedAt: time.Now().UTC(), Tables: map[string]int64{}}
	var plain bytes.Buffer
	gz := gzip.NewWriter(&plain)
	tw := tar.NewWriter(gz)
	add := func(name string, data []byte, mode int64) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(data)), ModTime: sum.CreatedAt, Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	// 所有表在同一个只读事务里读取，得到一致的快照（不会出现版本已写入、发布记录还没有的半截状态）。
	// PostgreSQL 默认 READ COMMITTED 每条语句各看一次，需要 REPEATABLE READ。
	txOpt := &sql.TxOptions{}
	if st.DB.Name() == "postgres" {
		txOpt = &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}
	}
	err := st.DB.Transaction(func(tx *gorm.DB) error {
		for _, t := range tables() {
			if t.name == "metric_minutes" && !opt.Metrics {
				sum.Tables[t.name] = 0
				continue
			}
			var b bytes.Buffer
			n, err := t.dump(tx, &b)
			if err != nil {
				return fmt.Errorf("export %s: %w", t.name, err)
			}
			sum.Tables[t.name] = n
			if err := add("db/"+t.name+".gob", b.Bytes(), 0o600); err != nil {
				return err
			}
		}
		return nil
	}, txOpt)
	if err != nil {
		return nil, err
	}
	for _, f := range dataFiles {
		b, err := os.ReadFile(filepath.Join(dataDir, filepath.FromSlash(f)))
		if errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		if err := add("files/"+f, b, 0o600); err != nil {
			return nil, err
		}
		sum.Files = append(sum.Files, f)
	}
	mj, _ := json.MarshalIndent(sum, "", "  ")
	if err := add("manifest.json", mj, 0o644); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}

	salt, nonce := make([]byte, 16), make([]byte, 12)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	blk, err := aes.NewCipher(deriveKey(pass, salt))
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	header := append(append([]byte(magic), salt...), nonce...)
	if _, err := w.Write(header); err != nil {
		return nil, err
	}
	if _, err := w.Write(aead.Seal(nil, nonce, plain.Bytes(), header)); err != nil {
		return nil, err
	}
	_ = st.SetKV("last_backup_at", sum.CreatedAt.Format(time.RFC3339))
	return sum, nil
}

func unpackArchive(r io.Reader, pass []byte) (map[string][]byte, *Summary, error) {
	raw, err := io.ReadAll(io.LimitReader(r, 8<<30))
	if err != nil {
		return nil, nil, err
	}
	if len(raw) < len(magic)+28 || string(raw[:len(magic)]) != magic {
		return nil, nil, errors.New("不是 " + brand.Product() + " 备份文件")
	}
	header := raw[:len(magic)+28]
	salt, nonce := header[len(magic):len(magic)+16], header[len(magic)+16:]
	blk, err := aes.NewCipher(deriveKey(pass, salt))
	if err != nil {
		return nil, nil, err
	}
	aead, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, nil, err
	}
	plain, err := aead.Open(nil, nonce, raw[len(header):], header)
	if err != nil {
		return nil, nil, errors.New("无法解密：口令错误或备份文件已损坏")
	}
	gz, err := gzip.NewReader(bytes.NewReader(plain))
	if err != nil {
		return nil, nil, err
	}
	entries := map[string][]byte{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		clean := path.Clean(h.Name)
		if h.Typeflag != tar.TypeReg || clean != h.Name || strings.HasPrefix(clean, "/") || strings.HasPrefix(clean, "..") {
			return nil, nil, fmt.Errorf("备份中有不安全的路径：%s", h.Name)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return nil, nil, err
		}
		entries[clean] = b
	}
	var sum Summary
	if err := json.Unmarshal(entries["manifest.json"], &sum); err != nil {
		return nil, nil, errors.New("备份缺少清单")
	}
	return entries, &sum, nil
}

// Verify 校验备份文件完整性：解密、校验清单、逐表反序列化 gob 并核对行数，不修改任何数据库或磁盘文件。
func Verify(r io.Reader, pass []byte) (*Summary, error) {
	entries, sum, err := unpackArchive(r, pass)
	if err != nil {
		return nil, err
	}
	for _, t := range tables() {
		b, ok := entries["db/"+t.name+".gob"]
		if !ok {
			if sum.Tables[t.name] > 0 {
				return nil, fmt.Errorf("备份缺少表 %s 的数据文件（清单记录 %d 行）", t.name, sum.Tables[t.name])
			}
			continue
		}
		n, err := t.verify(bytes.NewReader(b))
		if err != nil {
			return nil, fmt.Errorf("verify %s: %w", t.name, err)
		}
		if want, ok := sum.Tables[t.name]; ok && n != want {
			return nil, fmt.Errorf("表 %s 行数与清单不一致：解码 %d 行，清单 %d 行", t.name, n, want)
		}
	}
	for _, f := range sum.Files {
		if _, ok := entries["files/"+f]; !ok {
			return nil, fmt.Errorf("备份缺少文件 files/%s", f)
		}
	}
	return sum, nil
}

// Prune 清理备份目录中的旧备份（*.etosbk）：保留最新的 keepLast 份，并保留 keepDays 天内的备份。
// 返回被删除的文件路径列表。
func Prune(dir string, keepLast, keepDays int, now time.Time) ([]string, error) {
	if keepLast <= 0 && keepDays <= 0 {
		return nil, errors.New("至少指定 keepLast 或 keepDays 之一")
	}
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	type item struct {
		path    string
		modTime time.Time
	}
	var items []item
	for _, de := range des {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".etosbk") {
			continue
		}
		fi, err := de.Info()
		if err != nil {
			continue
		}
		items = append(items, item{path: filepath.Join(dir, de.Name()), modTime: fi.ModTime()})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].modTime.After(items[j].modTime)
	})
	var removed []string
	for idx, it := range items {
		if keepLast > 0 && idx < keepLast {
			continue
		}
		if keepDays > 0 && now.Sub(it.modTime) < time.Duration(keepDays)*24*time.Hour {
			continue
		}
		if err := os.Remove(it.path); err != nil {
			return removed, err
		}
		removed = append(removed, it.path)
	}
	return removed, nil
}

// Restore 把备份恢复到空库与数据目录。force 为 true 时覆盖数据目录中已有的密钥文件。
func Restore(r io.Reader, st *store.Store, dataDir string, pass []byte, force bool) (*Summary, error) {
	entries, sumPtr, err := unpackArchive(r, pass)
	if err != nil {
		return nil, err
	}
	sum := *sumPtr

	// 目标必须是空库：恢复会保留原有主键，不能与已有数据混在一起
	// 只启动过、未初始化的控制台会在 system_kvs 里生成启动密钥，恢复时以备份为准覆盖。
	for _, t := range tables() {
		if t.name == "system_kvs" {
			continue
		}
		var n int64
		if err := st.DB.Model(t.model).Count(&n).Error; err != nil {
			return nil, err
		}
		if n > 0 {
			return nil, fmt.Errorf("目标数据库不是空库（%s 有 %d 行）：请恢复到新建的数据库", t.name, n)
		}
	}
	if !force {
		for _, f := range dataFiles {
			if _, err := os.Stat(filepath.Join(dataDir, filepath.FromSlash(f))); err == nil {
				if _, inBackup := entries["files/"+f]; inBackup {
					return nil, fmt.Errorf("数据目录中已有 %s；确认要覆盖时加 -force", f)
				}
			}
		}
	}

	// 先把密钥文件写成同目录下的临时文件：任何一个写不进去都在碰数据库之前失败，
	// 数据库与数据目录保持原样，排除故障后可以直接重试（不会因为库里已有数据而被“不是空库”拒绝）。
	staged, err := stageDataFiles(entries, dataDir)
	if err != nil {
		return nil, err
	}

	err = st.DB.Transaction(func(tx *gorm.DB) error {
		if _, ok := entries["db/system_kvs.gob"]; ok {
			if err := tx.Where("1 = 1").Delete(&store.SystemKV{}).Error; err != nil {
				return err
			}
		}
		for _, t := range tables() {
			b, ok := entries["db/"+t.name+".gob"]
			if !ok {
				continue
			}
			if _, err := t.load(tx, bytes.NewReader(b)); err != nil {
				return fmt.Errorf("import %s: %w", t.name, err)
			}
		}
		return resetSequences(tx)
	})
	if err != nil {
		staged.discard()
		return nil, err
	}
	// 数据库已提交：把临时文件改名到位（同目录改名，原子且几乎不会失败）
	if err := staged.commit(); err != nil {
		return nil, fmt.Errorf("数据库已恢复，但密钥文件未能全部就位（%w）：已就位的文件保留，其余临时文件已清理；请修复数据目录权限，并从同一份备份里手工取出缺少的 files/ 下的文件放到数据目录", err)
	}
	return &sum, nil
}

// stagedFiles 是已写好、尚未改名到位的密钥文件。
type stagedFiles struct{ items []stagedFile }
type stagedFile struct{ tmp, dst string }

func stageDataFiles(entries map[string][]byte, dataDir string) (*stagedFiles, error) {
	sf := &stagedFiles{}
	for _, f := range dataFiles {
		b, ok := entries["files/"+f]
		if !ok {
			continue
		}
		dst := filepath.Join(dataDir, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			sf.discard()
			return nil, err
		}
		mode := os.FileMode(0o600)
		if strings.HasSuffix(f, ".crt") {
			mode = 0o644
		}
		tmp, err := writeTemp(filepath.Dir(dst), b, mode)
		if err != nil {
			sf.discard()
			return nil, err
		}
		sf.items = append(sf.items, stagedFile{tmp: tmp, dst: dst})
	}
	return sf, nil
}

func writeTemp(dir string, b []byte, mode os.FileMode) (string, error) {
	t, err := os.CreateTemp(dir, ".restore-*")
	if err != nil {
		return "", err
	}
	name := t.Name()
	if _, err := t.Write(b); err != nil {
		_ = t.Close()
		_ = os.Remove(name)
		return "", err
	}
	if err := t.Sync(); err != nil {
		_ = t.Close()
		_ = os.Remove(name)
		return "", err
	}
	if err := t.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	if err := os.Chmod(name, mode); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

// discard 删除全部临时文件，并清理为暂存而新建的空目录之外的残留（目录保留，不影响重试）。
func (sf *stagedFiles) discard() {
	for _, it := range sf.items {
		_ = os.Remove(it.tmp)
	}
	sf.items = nil
}

func (sf *stagedFiles) commit() error {
	for i, it := range sf.items {
		if err := os.Rename(it.tmp, it.dst); err != nil {
			for _, rest := range sf.items[i:] {
				_ = os.Remove(rest.tmp)
			}
			return err
		}
	}
	return nil
}

// resetSequences：PostgreSQL 的自增序列不会因为显式写入主键而前进，恢复后需要对齐到最大值。
func resetSequences(tx *gorm.DB) error {
	if tx.Name() != "postgres" {
		return nil
	}
	for _, t := range tables() {
		stmt := &gorm.Statement{DB: tx}
		if err := stmt.Parse(t.model); err != nil {
			return err
		}
		pk := stmt.Schema.PrioritizedPrimaryField
		if pk == nil || pk.DataType == schema.String || !pk.AutoIncrement {
			continue
		}
		q := fmt.Sprintf("SELECT setval(pg_get_serial_sequence(?, ?), COALESCE((SELECT MAX(%s) FROM %s), 0) + 1, false)",
			tx.Statement.Quote(pk.DBName), tx.Statement.Quote(stmt.Schema.Table))
		if err := tx.Exec(q, stmt.Schema.Table, pk.DBName).Error; err != nil {
			return fmt.Errorf("reset sequence %s: %w", stmt.Schema.Table, err)
		}
	}
	return nil
}

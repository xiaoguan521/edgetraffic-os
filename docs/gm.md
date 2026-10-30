# 国密 TLCP（P8 国密变体）

需要授权功能 `gm_tls`（企业版）。

## 方案

- **数据面**：`dataplane/build-syapp.sh -t tongsuo` 用铜锁（Tongsuo，Apache-2.0，版本与提交哈希见 `dataplane/UPSTREAM`）替换 OpenSSL：
  - 开启 NTLS，静态链接；
  - syapp 源码与改名 patch **完全不变**；
  - configure 带 `--build=gmssl`，因此 `syapp -v` 显示 `syapp/<版本> (gmssl)`，`syapp -V` 的配置参数中带 `enable-ntls`。
- **配置**：不需要改 syapp 源码。国密双证书通过 syapp 已有的 `ssl_conf_command` 指令交给铜锁的 SSL_CONF 命令：

  ```
  ssl_certificate      certs/site-gov.crt;          # 标准证书：服务不支持国密的客户端
  ssl_certificate_key  certs/site-gov.key;
  ssl_ciphers       "ECC-SM2-SM4-GCM-SM3:ECC-SM2-SM4-CBC-SM3:ECDHE-SM2-SM4-GCM-SM3:ECDHE-SM2-SM4-CBC-SM3:HIGH:!aNULL:!MD5";
  ssl_conf_command  Enable_ntls  on;
  ssl_conf_command  SignCertificate  /opt/syapp/conf/releases/r000123/certs/site-gov.sm2-sign.crt;
  ssl_conf_command  SignPrivateKey   /opt/syapp/conf/releases/r000123/certs/site-gov.sm2-sign.key;
  ssl_conf_command  EncCertificate   /opt/syapp/conf/releases/r000123/certs/site-gov.sm2-enc.crt;
  ssl_conf_command  EncPrivateKey    /opt/syapp/conf/releases/r000123/certs/site-gov.sm2-enc.key;
  ```

  - `ssl_conf_command` 的文件路径不按配置目录解析，所以渲染器写**本版本目录**的绝对路径；版本目录仍然自包含。
  - 证书续期时，渲染器把这些路径改写为新版本号（`render.RebaseRevision`）。agent 的“只换证书”校验只容许这一处精确改写，因此授权只读时国密证书仍可续期。
  - 默认 443 server（`ssl_reject_handshake`）在有国密站点时同样开启 `Enable_ntls`：国密握手在 SNI 切换前识别。
  - 和标准 TLS 一样，不带 SNI 的国密握手会被拒绝。

## 控制台

1. **证书 → 上传国密证书**：上传签名证书 + 私钥、加密证书 + 私钥（PEM，可带中间证书）。控制台会校验：
   - 两张都是 SM2 证书；
   - 私钥与证书匹配（SM2 曲线上计算公钥比对）；
   - 密钥用途正确，签名证书与加密证书放反会直接提示；
   - 证书未过期，签名证书覆盖站点域名。

   私钥加密保存，不回显。两张证书中较早到期的一张决定到期提醒。
2. **站点 → HTTPS**：标准证书必选，“国密证书”可选。
3. **发布前检查**：
   - 授权必须包含 `gm_tls`；
   - 节点组内所有节点都必须运行国密版 syapp。节点列表中带“国密”标签的即是；控制台按 `syapp -V` 中的 `enable-ntls` 识别。

   不满足时预览和发布报 `GM_VARIANT_REQUIRED`，并列出节点名。即使绕过检查，标准版 `syapp -t` 也会拒绝 `Enable_ntls`，agent 会自动回滚。
4. **数据面升级**：有国密站点的节点组只能升级到 `gmssl` 变体。

## 发布与升级包

- `v*` 标签的 Release 会构建 `syapp-<版本>-gmssl-<架构>.tar.gz`（amd64 / arm64，glibc 2.17 起）。
- 厂商在离线签名机上签名升级包：

  ```
  syapp-license-signer sign-package -key etos-pkg-*.key.json -in syapp-<版本>-gmssl-amd64.tar.gz \
    -name syapp -version <版本> -arch amd64 -variant gmssl
  ```

- 客户把升级包导入“数据面版本库”，然后把节点组分批升级到国密版（热升级，不停服务）。

## 验收

CI 的 `dataplane-gm` 任务（`scripts/ci/gm-handshake.sh`）依次验证：

1. 渲染器的国密配置通过真实国密版 `syapp -t`（两个国密站点共用 443）；
2. 启动后，铜锁 `s_client -enable_ntls -ntls` 完成 ECC-SM2-SM4-GCM-SM3 / CBC-SM3 握手；
3. 按 SNI 拿到各站点自己的 SM2 证书；
4. 同一端口的标准客户端仍协商 TLS 1.3。

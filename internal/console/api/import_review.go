package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

// importReview is a live check of saved drafts, not a claim that unsupported
// source directives have been migrated or that a node has run syedge -t.
type importReviewSite struct {
	ID                 uint      `json:"id,omitempty"`
	SourceName         string    `json:"source_name,omitempty"`
	Name               string    `json:"name"`
	Missing            bool      `json:"missing"`
	IdentityUnverified bool      `json:"identity_unverified,omitempty"`
	Valid              bool      `json:"valid"`
	ValidationError    string    `json:"validation_error,omitempty"`
	Site               *siteView `json:"site,omitempty"`
}

type importReviewSnippet struct {
	ID     uint   `json:"id"`
	SiteID uint   `json:"site_id,omitempty"`
	Status string `json:"status"` // pending, approved, unavailable, missing
}

func (s *Server) reviewImport(c *gin.Context) {
	row, _, ok := s.loadImport(c)
	if !ok {
		return
	}
	if row.AppliedAt == nil {
		fail(c, http.StatusConflict, "NOT_APPLIED", "请先应用为草稿")
		return
	}
	if row.Result == "" {
		fail(c, http.StatusConflict, "APPLY_IN_PROGRESS", "导入应用尚未完成，请稍后重试")
		return
	}
	var applied ApplyResult
	if err := json.Unmarshal([]byte(row.Result), &applied); err != nil {
		failErr(c, err)
		return
	}
	ctx, q := c.Request.Context(), s.q()
	sites := make([]importReviewSite, 0, len(applied.Sites))
	for _, item := range applied.Sites {
		r := importReviewSite{ID: item.ID, SourceName: item.SourceName, Name: item.Name}
		if item.ID == 0 {
			// Old results did not record stable identities. A same-name site may
			// have been deleted and replaced; do not silently claim it is the import.
			r.IdentityUnverified = true
			sites = append(sites, r)
			continue
		}
		st, err := q.Site.WithContext(ctx).Where(q.Site.ID.Eq(item.ID), q.Site.GroupID.Eq(row.GroupID)).First()
		if errors.Is(err, gorm.ErrRecordNotFound) {
			r.Missing = true
		} else if err != nil {
			failErr(c, err)
			return
		} else {
			view := toSiteView(*st)
			r.Site = &view
			var spec render.Site
			err = json.Unmarshal([]byte(st.Spec), &spec)
			if err == nil {
				// A disabled draft is not included in Deploy.Input, so resolve its
				// bundles explicitly before validating the proposed site.
				for i := range spec.Routes {
					static := spec.Routes[i].Static
					if static == nil || static.BundleID == 0 {
						continue
					}
					bundle, e := q.StaticBundle.WithContext(ctx).Where(q.StaticBundle.ID.Eq(static.BundleID), q.StaticBundle.GroupID.Eq(row.GroupID)).First()
					if e != nil {
						err = fmt.Errorf("静态资源包 #%d 不存在或不属于该节点组", static.BundleID)
						break
					}
					static.Files, err = render.ReadTarGz(bundle.Data)
					if err != nil {
						break
					}
				}
			}
			if err == nil {
				err = s.validateWith(row.GroupID, &spec, nil, "", "")
			}
			r.Valid = err == nil
			if err != nil {
				r.ValidationError = err.Error()
			}
		}
		sites = append(sites, r)
	}
	snippets := make([]importReviewSnippet, 0, len(applied.Snippets))
	for _, id := range applied.Snippets {
		r := importReviewSnippet{ID: id}
		sn, err := q.Snippet.WithContext(ctx).Where(q.Snippet.ID.Eq(id), q.Snippet.GroupID.Eq(row.GroupID)).First()
		if errors.Is(err, gorm.ErrRecordNotFound) {
			r.Status = "missing"
		} else if err != nil {
			failErr(c, err)
			return
		} else {
			r.SiteID = sn.SiteID
			switch {
			case sn.PendingBy != "":
				r.Status = "pending"
			case sn.Content != "":
				r.Status = "approved"
			default:
				r.Status = "unavailable"
			}
		}
		snippets = append(snippets, r)
	}
	c.JSON(http.StatusOK, gin.H{"checked_at": time.Now(), "sites": sites, "snippets": snippets})
}

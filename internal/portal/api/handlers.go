package api

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/xiaoguan521/edgetraffic-os/internal/portal/service"
)

func (s *Server) dashboard(c *gin.Context) {
	d, err := s.Svc.Dashboard(c.Request.Context())
	reply(c, d, err)
}

// ---------- 客户与订单 ----------

type customerReq struct {
	Code       string `json:"code"`
	Name       string `json:"name"`
	Contact    string `json:"contact"`
	Email      string `json:"email"`
	Phone      string `json:"phone"`
	SalesOwner string `json:"sales_owner"`
	Note       string `json:"note"`
}

func (r customerReq) in() service.CustomerInput {
	return service.CustomerInput{Code: r.Code, Name: r.Name, Contact: r.Contact, Email: r.Email, Phone: r.Phone, SalesOwner: r.SalesOwner, Note: r.Note}
}

func (s *Server) listCustomers(c *gin.Context) {
	v, err := s.Svc.ListCustomers(c.Request.Context(), c.Query("q"))
	reply(c, v, err)
}

func (s *Server) createCustomer(c *gin.Context) {
	var req customerReq
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	v, err := s.Svc.CreateCustomer(c.Request.Context(), actor(c), req.in())
	reply(c, v, err)
}

func (s *Server) updateCustomer(c *gin.Context) {
	i, ok := id(c)
	if !ok {
		return
	}
	var req customerReq
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	reply(c, nil, s.Svc.UpdateCustomer(c.Request.Context(), actor(c), i, req.in()))
}

func (s *Server) listOrders(c *gin.Context) {
	v, err := s.Svc.ListOrders(c.Request.Context(), queryUint(c, "customer_id"))
	reply(c, v, err)
}

func (s *Server) createOrder(c *gin.Context) {
	var req struct {
		OrderNo    string `json:"order_no"`
		CustomerID uint   `json:"customer_id"`
		Edition    string `json:"edition"`
		Nodes      int    `json:"nodes"`
		Sites      int    `json:"sites"`
		Days       int    `json:"days"`
		GraceDays  int    `json:"grace_days"`
		Amount     string `json:"amount"`
		ContractNo string `json:"contract_no"`
		Note       string `json:"note"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	v, err := s.Svc.CreateOrder(c.Request.Context(), actor(c), service.OrderInput{OrderNo: req.OrderNo, CustomerID: req.CustomerID,
		Edition: req.Edition, Nodes: req.Nodes, Sites: req.Sites, Days: req.Days, GraceDays: req.GraceDays, Amount: req.Amount,
		ContractNo: req.ContractNo, Note: req.Note})
	reply(c, v, err)
}

// ---------- License ----------

func (s *Server) listLicenses(c *gin.Context) {
	v, err := s.Svc.ListLicenses(c.Request.Context(), service.LicenseFilter{Status: c.Query("status"), CustomerID: queryUint(c, "customer_id")})
	reply(c, v, err)
}

func (s *Server) getLicense(c *gin.Context) {
	i, ok := id(c)
	if !ok {
		return
	}
	v, err := s.Svc.GetLicense(c.Request.Context(), i)
	reply(c, v, err)
}

func (s *Server) createDraft(c *gin.Context) {
	var req struct {
		OrderID           uint   `json:"order_id"`
		ActivationRequest string `json:"activation_request"`
		InstallationID    string `json:"installation_id"`
		Binding           string `json:"binding"`
		ExpiresAt         string `json:"expires_at"` // 可选，YYYY-MM-DD
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	in := service.DraftInput{OrderID: req.OrderID, ActivationRequest: req.ActivationRequest, InstallationID: req.InstallationID, Binding: req.Binding}
	if req.ExpiresAt != "" {
		t, err := time.Parse("2006-01-02", req.ExpiresAt)
		if err != nil {
			fail(c, http.StatusBadRequest, "BAD_DATE", "到期日格式应为 YYYY-MM-DD")
			return
		}
		in.ExpiresAt = t
	}
	v, err := s.Svc.CreateDraft(c.Request.Context(), actor(c), in)
	reply(c, v, err)
}

func (s *Server) deleteDraft(c *gin.Context) {
	if i, ok := id(c); ok {
		reply(c, nil, s.Svc.DeleteDraft(c.Request.Context(), actor(c), i))
	}
}

func (s *Server) submit(c *gin.Context) {
	if i, ok := id(c); ok {
		reply(c, nil, s.Svc.Submit(c.Request.Context(), actor(c), i))
	}
}

func (s *Server) approve(c *gin.Context) {
	if i, ok := id(c); ok {
		reply(c, nil, s.Svc.Approve(c.Request.Context(), actor(c), i))
	}
}

type reasonReq struct {
	Reason string `json:"reason"`
}

func (s *Server) reject(c *gin.Context) {
	i, ok := id(c)
	if !ok {
		return
	}
	var req reasonReq
	_ = c.ShouldBindJSON(&req)
	reply(c, nil, s.Svc.Reject(c.Request.Context(), actor(c), i, req.Reason))
}

func (s *Server) revoke(c *gin.Context) {
	i, ok := id(c)
	if !ok {
		return
	}
	var req reasonReq
	_ = c.ShouldBindJSON(&req)
	reply(c, nil, s.Svc.Revoke(c.Request.Context(), actor(c), i, req.Reason))
}

func (s *Server) renew(c *gin.Context) {
	i, ok := id(c)
	if !ok {
		return
	}
	var req struct {
		OrderID uint `json:"order_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	v, err := s.Svc.Renew(c.Request.Context(), actor(c), i, req.OrderID)
	reply(c, v, err)
}

func (s *Server) download(c *gin.Context) {
	i, ok := id(c)
	if !ok {
		return
	}
	name, body, err := s.Svc.Download(c.Request.Context(), actor(c), i)
	if err != nil {
		failErr(c, err)
		return
	}
	attachment(c, name, "text/plain; charset=utf-8", body)
}

// ---------- 签名批次、吊销 ----------

func (s *Server) listBatches(c *gin.Context) {
	v, err := s.Svc.ListBatches(c.Request.Context())
	reply(c, v, err)
}

func (s *Server) exportBatch(c *gin.Context) {
	var req struct {
		KID        string `json:"kid"`
		LicenseIDs []uint `json:"license_ids"`
		IncludeCRL bool   `json:"include_crl"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	v, err := s.Svc.ExportBatch(c.Request.Context(), actor(c), service.ExportInput{KID: req.KID, LicenseIDs: req.LicenseIDs, IncludeCRL: req.IncludeCRL})
	reply(c, v, err)
}

func (s *Server) batchRequest(c *gin.Context) {
	i, ok := id(c)
	if !ok {
		return
	}
	b, err := s.Svc.BatchRequest(c.Request.Context(), actor(c), i)
	if err != nil {
		failErr(c, err)
		return
	}
	attachment(c, "sign-request-"+b.BatchNo+".json", "application/json", b.Request)
}

func (s *Server) cancelBatch(c *gin.Context) {
	if i, ok := id(c); ok {
		reply(c, nil, s.Svc.CancelBatch(c.Request.Context(), actor(c), i))
	}
}

func (s *Server) importSigned(c *gin.Context) {
	b, err := readBody(c)
	if err != nil {
		failErr(c, err)
		return
	}
	v, err := s.Svc.ImportSigned(c.Request.Context(), actor(c), b)
	reply(c, v, err)
}

func (s *Server) listRevocations(c *gin.Context) {
	v, err := s.Svc.ListRevocations(c.Request.Context())
	reply(c, v, err)
}

func (s *Server) listCRLs(c *gin.Context) {
	v, err := s.Svc.ListCRLs(c.Request.Context())
	reply(c, v, err)
}

func (s *Server) downloadCRL(c *gin.Context) {
	crl, err := s.Svc.LatestCRL(c.Request.Context())
	if err != nil {
		failErr(c, err)
		return
	}
	if crl == nil {
		fail(c, http.StatusNotFound, "NOT_FOUND", "还没有发布过吊销列表")
		return
	}
	attachment(c, fmt.Sprintf("crl-%d.crl", crl.Serial), "text/plain; charset=utf-8", crl.Raw+"\n")
}

// ---------- 签名公钥、用户、审计 ----------

func (s *Server) listKeys(c *gin.Context) {
	v, err := s.Svc.ListKeys(c.Request.Context())
	reply(c, v, err)
}

func (s *Server) addKey(c *gin.Context) {
	var req struct {
		KID       string `json:"kid"`
		PublicKey string `json:"public_key"`
		Note      string `json:"note"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	v, err := s.Svc.AddKey(c.Request.Context(), actor(c), req.KID, req.PublicKey, req.Note)
	reply(c, v, err)
}

func (s *Server) keyStatus(c *gin.Context) {
	i, ok := id(c)
	if !ok {
		return
	}
	var req struct {
		Status string `json:"status"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	reply(c, nil, s.Svc.SetKeyStatus(c.Request.Context(), actor(c), i, req.Status))
}

func (s *Server) listUsers(c *gin.Context) {
	if err := actorNeedAdmin(c); err != nil {
		failErr(c, err)
		return
	}
	v, err := s.Svc.ListUsers(c.Request.Context())
	reply(c, v, err)
}

func actorNeedAdmin(c *gin.Context) error {
	if !actor(c).Has("admin") {
		return service.ErrForbidden
	}
	return nil
}

type userReq struct {
	Username    string   `json:"username"`
	DisplayName string   `json:"display_name"`
	Password    string   `json:"password"`
	Roles       []string `json:"roles"`
	Disabled    bool     `json:"disabled"`
}

func (s *Server) createUser(c *gin.Context) {
	var req userReq
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	v, err := s.Svc.CreateUser(c.Request.Context(), actor(c), req.Username, req.DisplayName, req.Password, req.Roles)
	reply(c, v, err)
}

func (s *Server) updateUser(c *gin.Context) {
	i, ok := id(c)
	if !ok {
		return
	}
	var req userReq
	if err := c.ShouldBindJSON(&req); err != nil {
		failErr(c, err)
		return
	}
	reply(c, nil, s.Svc.UpdateUser(c.Request.Context(), actor(c), i, req.Roles, req.Disabled, req.Password))
}

func (s *Server) listAudit(c *gin.Context) {
	n, _ := strconv.Atoi(c.DefaultQuery("limit", "200"))
	v, err := s.Svc.ListAudit(c.Request.Context(), n)
	reply(c, v, err)
}

func (s *Server) verifyAudit(c *gin.Context) {
	bad, err := s.Svc.VerifyAudit(c.Request.Context())
	if err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"intact": bad == 0, "first_bad_id": bad})
}

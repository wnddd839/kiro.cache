package server

import (
	"cmp"
	"encoding/json"
	"net/http"
	"strings"

	"kiro-go/internal/kiro"
	"kiro-go/internal/pool"
)

// adminMux 是号池管理接口。响应里不含 token。
//
//	GET    /admin/accounts                 列表（含冷却、额度、cache 命中率）
//	POST   /admin/accounts                 加号：{"label","cred":{...}} 或 {"import":"ide"}
//	DELETE /admin/accounts/{id}            删号
//	POST   /admin/accounts/{id}/enable     启用（清冷却）
//	POST   /admin/accounts/{id}/disable    停用
//	POST   /admin/accounts/{id}/refresh    强刷 token 并拉额度
//	GET    /admin/stats                    全池累计
func (s *Server) adminMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/accounts", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"accounts": s.pool.List()})
	})
	mux.HandleFunc("POST /admin/accounts", s.addAccount)
	mux.HandleFunc("DELETE /admin/accounts/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := s.pool.Remove(r.PathValue("id")); err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /admin/accounts/{id}/{action}", s.accountAction)
	mux.HandleFunc("GET /admin/stats", func(w http.ResponseWriter, r *http.Request) {
		t := s.pool.Totals()
		writeJSON(w, http.StatusOK, map[string]any{"totals": t, "cache_hit_rate": t.HitRate(), "accounts": s.pool.Len()})
	})
	return mux
}

type addRequest struct {
	ID            string    `json:"id"`
	Label         string    `json:"label"`
	MaxConcurrent int       `json:"max_concurrent"`
	Import        string    `json:"import"` // "ide"：从 Kiro IDE 的 token 文件导入
	Path          string    `json:"path"`   // 覆盖默认 IDE token 路径
	Cred          kiro.Cred `json:"cred"`
}

func (s *Server) addAccount(w http.ResponseWriter, r *http.Request) {
	var in addRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	a := pool.Account{ID: in.ID, Label: in.Label, MaxConcurrent: in.MaxConcurrent, Cred: in.Cred}
	switch strings.ToLower(in.Import) {
	case "":
		if strings.HasPrefix(a.Cred.AccessToken, "ksk_") && a.Cred.Method == "" {
			a.Cred.Method = kiro.MethodAPIKey
		}
	case "ide":
		p := in.Path
		if p == "" {
			var err error
			if p, err = kiro.IDETokenPath(); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
		cred, err := kiro.ReadIDE(p)
		if err != nil {
			writeError(w, http.StatusBadRequest, "read IDE sign-in: "+err.Error())
			return
		}
		a.Cred, a.Source, a.SourcePath = cred, pool.SourceIDE, p
		a.Label = cmp.Or(a.Label, "kiro-ide")
	default:
		writeError(w, http.StatusBadRequest, `import must be "ide" or empty`)
		return
	}
	added, err := s.pool.Add(a)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// 补 profile、拉额度与邮箱。失败不影响加号，只在响应里提示
	resp := map[string]any{"id": added.ID}
	if _, err := s.pool.RefreshLimits(r.Context(), added.ID); err != nil {
		resp["warning"] = err.Error()
	}
	if v, ok := s.pool.Get(added.ID); ok {
		resp["account"] = v
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) accountAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.pool.Get(id); !ok {
		writeError(w, http.StatusNotFound, "account not found")
		return
	}
	var err error
	switch r.PathValue("action") {
	case "enable":
		err = s.pool.SetDisabled(id, false, "")
	case "disable":
		err = s.pool.SetDisabled(id, true, "disabled by admin")
	case "refresh":
		if _, err = s.pool.ForceRefresh(r.Context(), id); err == nil {
			_, err = s.pool.RefreshLimits(r.Context(), id)
		}
	default:
		writeError(w, http.StatusNotFound, "unknown action")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	v, _ := s.pool.Get(id)
	writeJSON(w, http.StatusOK, v)
}

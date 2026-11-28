package domains

import (
	"net/http"
	"strings"

	appctx "gist.github.com/stefanpejcic/openpanel/internal/app"
	"gist.github.com/stefanpejcic/openpanel/internal/auth"
	"gist.github.com/stefanpejcic/openpanel/internal/core/logger"
	"gist.github.com/stefanpejcic/openpanel/internal/core/reqip"
	"gist.github.com/stefanpejcic/openpanel/internal/modules/cache"
	"gist.github.com/stefanpejcic/openpanel/internal/web"
)

// purgeDomainCache runs the purge for a domain the caller already checked and writes the activity log line
func purgeDomainCache(a *appctx.App, r *http.Request, userID int, domain, via string) (cache.PurgeResult, error) {
	ctx := r.Context()
	username, userContext, err := injected(a, ctx, userID)
	if err != nil {
		return cache.PurgeResult{Domain: domain}, err
	}
	res, err := cache.PurgeDomain(ctx, a, userContext, domain)
	if err != nil {
		return res, err
	}
	what := "nothing to purge"
	if names := res.Names(); len(names) > 0 {
		what = strings.Join(names, ", ")
	}
	_ = logger.RecordUserAction(a.Config, username, "purged cache for domain "+domain+via+" ("+what+")", reqip.ClientIP(r))
	return res, nil
}

// handleDomainPurgeCache is the Purge cache button in the domains table and its bulk action
func handleDomainPurgeCache(a *appctx.App, w http.ResponseWriter, r *http.Request) {
	userID, _ := auth.UserID(r)
	_ = r.ParseForm()
	domainName := strings.ToLower(strings.TrimSpace(r.Form.Get("domain_name")))

	if domainName == "" || strings.HasSuffix(domainName, ".onion") {
		web.FlashRedirect(a, w, r, "error", "Invalid request. Domain name must be provided.", "/domains")
		return
	}
	if !a.CheckExactDomainBelongsToUser(r.Context(), userID, domainName) {
		http.Error(w, "You do not own this domain.", http.StatusForbidden)
		return
	}

	res, err := purgeDomainCache(a, r, userID, domainName, "")
	if err != nil {
		web.FlashRedirect(a, w, r, "error", err.Error(), "/domains")
		return
	}
	web.FlashRedirect(a, w, r, "success", res.Message(web.RequestTranslator(a, r)), "/domains")
}

// apiDomainsPurgeCache is POST /api/domains/{domain}/purge-cache
func apiDomainsPurgeCache(a *appctx.App, w http.ResponseWriter, r *http.Request) {
	userID, _ := auth.UserID(r)
	domain := strings.ToLower(r.PathValue("domain"))
	if !apiOwnDomainOr403Domains(a, w, r, userID, domain) {
		return
	}
	if strings.HasSuffix(domain, ".onion") {
		writeAPIDomainsJSON(w, http.StatusBadRequest, map[string]string{"error": "Not available for .onion domains."})
		return
	}
	res, err := purgeDomainCache(a, r, userID, domain, " via API")
	if err != nil {
		writeAPIDomainsJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeAPIDomainsJSON(w, http.StatusOK, map[string]any{"domain": res.Domain, "purged": res.Purged(), "layers": res.Layers})
}

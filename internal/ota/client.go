package ota

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Client interroge l'API GitHub Releases et télécharge des assets.
//
// Le cache de CheckLatest est en RAM, par instance (pas de persistence
// disque) : un restart oblige à re-checker. Suffit largement vu qu'on
// ne check qu'à l'ouverture de l'UI.
//
// Deux http.Client séparés : `http` pour les appels API JSON (rapides,
// timeout 15s) et `downloadHTTP` pour les downloads d'assets (binaire
// ARM ~10 MB sur la cam à ~100 KB/s = ~100s, timeout 10 min pour avoir
// de la marge).
type Client struct {
	repo         string        // ex "Caligone/openqiara"
	currentVer   string        // version courante du binaire qui tourne
	http         *http.Client  // API JSON: CheckLatest, fetchChecksum
	downloadHTTP *http.Client  // assets binaires: download de la release
	cacheTTL     time.Duration
	logger       *slog.Logger

	mu        sync.Mutex
	cached    *CheckResult // last result, may be nil
	cachedAt  time.Time
}

// NewClient construit un client OTA. currentVersion = ce que main.BuildInfo()
// retourne ("dev" si build local sans -ldflags).
func NewClient(currentVersion string, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		repo:         defaultRepo,
		currentVer:   currentVersion,
		http:         newHTTPClient(15 * time.Second),
		downloadHTTP: newHTTPClient(10 * time.Minute),
		cacheTTL:     time.Hour,
		logger:       logger,
	}
}

// CheckLatest retourne les infos sur la dernière release publiée. Mis en
// cache `cacheTTL` (1h par défaut) pour ne pas spammer l'API GitHub —
// celle-ci limite à 60 req/h en non-authentifié, et un check par
// ouverture UI est suffisant.
//
// force=true contourne le cache (bouton "Vérifier maintenant").
func (c *Client) CheckLatest(ctx context.Context, force bool) (*CheckResult, error) {
	c.mu.Lock()
	if !force && c.cached != nil && time.Since(c.cachedAt) < c.cacheTTL {
		cp := *c.cached
		c.mu.Unlock()
		return &cp, nil
	}
	c.mu.Unlock()

	// On utilise /releases (liste) plutôt que /releases/latest parce que ce
	// dernier exclut les pre-releases (prerelease=true / draft=true). Tant
	// qu'on est en alpha, toutes les tags sont prerelease, donc /latest
	// renverrait 404. La liste est triée par GitHub du plus récent au plus
	// ancien — on prend simplement le premier non-draft.
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases?per_page=10", c.repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build github request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("github fetch: status %d: %s", resp.StatusCode, body)
	}

	var list []Release
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("decode releases: %w", err)
	}

	// Aucune release publiée → pas d'update, pas une erreur.
	if len(list) == 0 {
		res := &CheckResult{
			Current:      c.currentVer,
			UpdateNeeded: false,
			CheckedAt:    time.Now().UTC().Format(time.RFC3339),
		}
		c.cacheResult(res)
		return res, nil
	}
	rel := list[0]

	res := &CheckResult{
		Current:      c.currentVer,
		Latest:       rel,
		UpdateNeeded: shouldOfferUpdate(c.currentVer, rel.TagName),
		CheckedAt:    time.Now().UTC().Format(time.RFC3339),
	}
	c.cacheResult(res)
	return res, nil
}

func (c *Client) cacheResult(r *CheckResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := *r
	c.cached = &cp
	c.cachedAt = time.Now()
}

// InvalidateCache force le prochain CheckLatest à refetch. Utile après
// un install réussi (la version courante a changé).
func (c *Client) InvalidateCache() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cached = nil
	c.cachedAt = time.Time{}
}

// shouldOfferUpdate décide si on propose la mise à jour à l'UI.
//
// Règles :
//   - current "dev" ou vide → pas d'update (build local, l'auteur sait ce
//     qu'il fait).
//   - current == latest tag → pas d'update.
//   - sinon → update.
//
// On ne compare PAS sémantiquement les versions (v0.1.0 vs v0.1.1). Une
// inégalité textuelle suffit : si tu tournes une version, qu'elle soit
// plus ancienne ou plus récente que la "latest" GitHub, on te le dit.
// Cas concret : tu as un build de dev "v0.1.0+local" et la release est
// v0.1.0 — l'UI proposera de "downgrader", tu peux refuser. Plus safe
// qu'un parser SemVer qui se trompe.
func shouldOfferUpdate(current, latest string) bool {
	if current == "" || current == "dev" {
		return false
	}
	if latest == "" {
		return false
	}
	// Extraire juste la partie tag de current (BuildInfo() peut renvoyer
	// "v0.1.0-alpha.1 (abc1234, 2026-05-15)").
	cur := current
	if i := indexByte(cur, ' '); i >= 0 {
		cur = cur[:i]
	}
	return cur != latest
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

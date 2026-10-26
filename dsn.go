package argus

import (
	"errors"
	"net/url"
	"strings"
)

// dsn is a parsed https://<publicKey>@<host>/<projectID> (spec §1).
type dsn struct {
	raw       string
	publicKey string
	projectID string
	// origin is scheme://host plus any path prefix before the project id.
	origin string
}

var errBadDSN = errors.New("argus: the DSN is not valid (want https://<key>@<host>/<project id>)")

func parseDSN(raw string) (*dsn, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User == nil {
		return nil, errBadDSN
	}
	key := u.User.Username()
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	project := parts[len(parts)-1]
	if key == "" || project == "" || strings.Trim(project, "0123456789") != "" {
		return nil, errBadDSN
	}
	prefix := ""
	if len(parts) > 1 {
		prefix = "/" + strings.Join(parts[:len(parts)-1], "/")
	}
	return &dsn{raw: raw, publicKey: key, projectID: project, origin: u.Scheme + "://" + u.Host + prefix}, nil
}

// envelopeURL is the error endpoint (spec §3).
func (d *dsn) envelopeURL() string { return d.origin + "/api/" + d.projectID + "/envelope/" }

// url is any other ingest endpoint; servers authenticate with a header.
func (d *dsn) url(path string) string { return d.origin + path }

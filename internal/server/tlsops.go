// TLS bootstrap surface: GET /ca.pem serves the generated CA so clients
// can fetch it with a one-time `curl -k`, and GET /tls-help renders the
// per-platform import walkthrough with this daemon's address baked in.
package server

import (
	"fmt"
	"html"
	"net/http"
	"os"

	"github.com/dncore/synapse-protocol-adapter/internal/tlscert"
)

// handleCACert serves the generated CA certificate (public by design —
// every client is supposed to import it). Only the CA is ever exposed;
// private keys have no HTTP path.
func (s *Server) handleCACert(w http.ResponseWriter, r *http.Request) {
	caPath := tlscert.CACertPath(s.cfg.Server.TLS)
	if caPath == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "CA download is available in auto-certificate mode; with server.tls.cert_file set, distribute that certificate yourself",
		})
		return
	}
	pem, err := os.ReadFile(caPath)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "CA certificate not generated yet"})
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-text")
	_, _ = w.Write(pem)
}

// handleTLSHelp renders the import tutorial. The request's own host is
// embedded so every copy-paste command targets this daemon exactly.
func (s *Server) handleTLSHelp(w http.ResponseWriter, r *http.Request) {
	host := html.EscapeString(r.Host)
	if host == "" {
		host = "<host>:<port>"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, tlsHelpHTML, host, host, host, host, host, host)
}

// tlsHelpHTML has exactly six %s verbs (the daemon address); they are
// filled with an html-escaped host in handleTLSHelp.
const tlsHelpHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>synapse — trust this server's certificate</title>
<style>
  body { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
         max-width: 780px; margin: 2rem auto; padding: 0 1rem;
         line-height: 1.55; color: #222; background: #fafafa; }
  h1 { font-size: 1.35rem; } h2 { font-size: 1.05rem; margin-top: 1.8rem; }
  pre { background: #eee; padding: .7rem 1rem; border-radius: 6px;
        overflow-x: auto; font-size: .85rem; }
  code { background: #eee; padding: .1rem .3rem; border-radius: 4px; }
  .note { color: #666; }
</style>
</head>
<body>
<h1>Trust this server's certificate</h1>
<p>This endpoint serves HTTPS with a certificate from the gateway's own
certificate authority. Import that CA <b>once</b> per client machine and
every connection verifies normally afterwards. The CA only changes if it
is regenerated (years); re-signing the server certificate (new domains,
renewal) keeps the same CA and needs <b>no</b> re-import.</p>

<h2>1. Download the CA</h2>
<pre>curl http://%s/ca.pem -o synapse-ca.pem</pre>
<p class="note">The gateway serves both plain HTTP and HTTPS on this port
(auto-detected per connection), so the one request you cannot yet
verify — fetching the CA — can simply go over HTTP. After importing it
below, use <code>https://</code> URLs and they verify for real.</p>

<h2>2. Import it — pick your OS</h2>

<h3>macOS</h3>
<pre>sudo security add-trusted-cert -d -r trustRoot \
  -k /Library/Keychains/System.keychain synapse-ca.pem</pre>
<p class="note"><code>security</code> exists only on macOS — run it on the
Mac, not on a Linux box. Fully quit and reopen GUI apps afterwards.</p>

<h3>Linux — Debian / Ubuntu</h3>
<pre>sudo cp synapse-ca.pem /usr/local/share/ca-certificates/synapse-ca.crt
sudo update-ca-certificates</pre>

<h3>Linux — Fedora / Arch</h3>
<pre>sudo trust anchor --store synapse-ca.pem</pre>

<h3>Windows (admin PowerShell)</h3>
<pre>Import-Certificate -FilePath synapse-ca.pem -CertStoreLocation Cert:\LocalMachine\Root</pre>
<p class="note">or <code>certutil -addstore -f ROOT synapse-ca.pem</code>
in a cmd window opened as Administrator.</p>

<h2>Without touching the system store</h2>
<p>Some clients accept a CA per process instead:</p>
<pre># Node / Electron CLI tools (Claude Code etc.)
export NODE_EXTRA_CA_CERTS=$PWD/synapse-ca.pem

# Python
export REQUESTS_CA_BUNDLE=$PWD/synapse-ca.pem

# curl, one-off
curl --cacert synapse-ca.pem https://%s/health</pre>
<p class="note">Env vars apply only to processes started with them set —
restart the agent in that shell.</p>

<h2>3. Verify</h2>
<pre>curl https://%s/health          # no -k: must return {"status":"ok"}
curl https://%s/version</pre>

<hr>
<p class="note">Gateway operators: add domains to the certificate with
<code>synapse tls add &lt;name&gt;</code> — served certificates pick the
change up live. This page: <code>https://%s/tls-help</code>,
CA: <code>https://%s/ca.pem</code>.</p>
</body>
</html>
`

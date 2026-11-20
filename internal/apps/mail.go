package apps

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// mailPorts pairs each mail port variable in the prod mail compose template
// with its standard port and the high port used while the standard one is
// taken. Order is the order written to _/.env.
var mailPorts = []struct {
	Var      string
	Std, Alt int
}{
	{"MAIL_SETUP_PORT", 8080, 18080},
	{"MAIL_SMTP_PORT", 25, 2525},
	{"MAIL_SUBMISSIONS_PORT", 465, 4650},
	{"MAIL_SUBMISSION_PORT", 587, 5870},
	{"MAIL_IMAP_PORT", 143, 1430},
	{"MAIL_IMAPS_PORT", 993, 9930},
	{"MAIL_POP3_PORT", 110, 1100},
	{"MAIL_POP3S_PORT", 995, 9950},
	{"MAIL_SIEVE_PORT", 4190, 14190},
}

// mailPortFree reports whether a standard mail port can be bound now. A var so
// tests can pretend another mail server holds the ports.
var mailPortFree = portFree

// writeMailEnv writes _/.env for a prod mail app — the file compose reads for
// the ${MAIL_*_PORT} substitutions. When any standard mail port is already held
// (another mail server, e.g. Mailcow, still serving during a migration), every
// port gets its high alternative so the new server can start beside the old one
// instead of failing to bind. All or nothing: a stack half on standard ports
// would grab the ones the old server isn't using and confuse clients.
//
// Cutover is then: stop the old server, delete the override lines (Env tab),
// restart the app.
func writeMailEnv(underscore string) error {
	busy := false
	for _, p := range mailPorts {
		if !mailPortFree(p.Std) {
			busy = true
			break
		}
	}
	var b strings.Builder
	b.WriteString("# Hostnames whose certificates (from the proxy) the mail server gets in\n")
	b.WriteString("# /etc/stalwart/tls/. Comma separated; unset = the app's admin domain.\n")
	b.WriteString("# MAIL_TLS_HOSTS=mail.example.com,mail2.example.com\n\n")
	b.WriteString("# Host ports the mail server publishes. Unset = the standard port.\n")
	if !busy {
		b.WriteString("# Override one by adding e.g. MAIL_SMTP_PORT=2525 here, then restart.\n")
		return os.WriteFile(filepath.Join(underscore, ".env"), []byte(b.String()), 0o644)
	}
	b.WriteString("# Standard mail ports were in use when this app was created (another mail\n")
	b.WriteString("# server still running), so it runs on high ports alongside it. CUTOVER: stop\n")
	b.WriteString("# the old server, delete the MAIL_*_PORT lines below, restart this app.\n")
	for _, p := range mailPorts {
		fmt.Fprintf(&b, "%s=%d\n", p.Var, p.Alt)
	}
	return os.WriteFile(filepath.Join(underscore, ".env"), []byte(b.String()), 0o644)
}

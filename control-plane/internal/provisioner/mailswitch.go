package provisioner

import (
	"context"
	"fmt"
	"strings"
)

// MailSwitchPath is where a site image keeps the must-use plugin module that
// suppresses wp_mail() when PRESSFLEET_DISABLE_MAIL is set (images/shared/
// mu-plugins/pressfleet-environment/mail-switch.php, §5.6). Images built
// before it existed ignore the constant, and image tags are rebuilt without
// changing, so a host's cached build of a tag can lack it. A staging site is
// a copy of production's customers, so staging creation looks for this file
// in the image the site runs before any production data reaches it (§5.11).
const MailSwitchPath = "/opt/pressfleet/mu-plugins/pressfleet-environment/mail-switch.php"

// Answers of the mail switch check.
const (
	mailSwitchPresent = "pressfleet-mail-switch:present"
	mailSwitchMissing = "pressfleet-mail-switch:missing"
)

// MailSwitchCheckArgs are the ExecWPCLI arguments that print whether
// MailSwitchPath exists in the site's PHP container. They skip loading
// WordPress, which a site that isn't installed yet can't do, and still run
// through the driver's ordinary command path, so any driver can answer them.
func MailSwitchCheckArgs() []string {
	return []string{"--skip-wordpress", "eval", fmt.Sprintf("echo is_file('%s') ? '%s' : '%s';", MailSwitchPath, mailSwitchPresent, mailSwitchMissing)}
}

// MailSwitchAnswer returns the output of the mail switch check for a site
// image that has the switch or lacks it. Fakes answer the check with it.
func MailSwitchAnswer(present bool) string {
	if present {
		return mailSwitchPresent
	}
	return mailSwitchMissing
}

// HasMailSwitch reports whether the site's PHP container has the platform's
// outbound mail switch. The site has to be running. An unexpected answer is
// an error, never "absent" or "present".
func HasMailSwitch(ctx context.Context, p Provisioner, siteID string) (bool, error) {
	out, err := p.ExecWPCLI(ctx, siteID, MailSwitchCheckArgs())
	if err != nil {
		return false, err
	}
	for line := range strings.Lines(out) {
		switch strings.TrimSpace(line) {
		case mailSwitchPresent:
			return true, nil
		case mailSwitchMissing:
			return false, nil
		}
	}
	return false, fmt.Errorf("the mail switch check printed %q", strings.TrimSpace(out))
}

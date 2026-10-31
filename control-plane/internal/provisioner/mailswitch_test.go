package provisioner_test

import (
	"context"
	"errors"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// execOnly is a Provisioner that answers ExecWPCLI.
type execOnly struct {
	provisioner.Provisioner
	out string
	err error
}

func (p execOnly) ExecWPCLI(context.Context, string, []string) (string, error) { return p.out, p.err }

func TestHasMailSwitch(t *testing.T) {
	boom := errors.New("boom")
	for _, tc := range []struct {
		name    string
		p       execOnly
		present bool
		wantErr bool
	}{
		{"present", execOnly{out: provisioner.MailSwitchAnswer(true)}, true, false},
		{"missing", execOnly{out: provisioner.MailSwitchAnswer(false)}, false, false},
		{"after a notice", execOnly{out: "PHP Notice: x\n" + provisioner.MailSwitchAnswer(true) + "\n"}, true, false},
		{"unexpected output", execOnly{out: "PHP Warning: something else"}, false, true},
		{"empty output", execOnly{}, false, true},
		{"exec error", execOnly{err: boom}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := provisioner.HasMailSwitch(context.Background(), tc.p, "site")
			if got != tc.present || (err != nil) != tc.wantErr {
				t.Errorf("HasMailSwitch = %v, %v; want %v, error %v", got, err, tc.present, tc.wantErr)
			}
		})
	}
}

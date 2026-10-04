package tmux

import (
	"strings"
	"testing"

	"github.com/lemonsaurus/agency/internal/config"
)

func TestBothConfigsRecordPaneDeaths(t *testing.T) {
	for name, conf := range map[string]string{
		"earth": buildTmuxConf(config.DefaultConfig(), "/bin/agency"),
		"cloud": buildCloudConf("/bin/agency"),
	} {
		for _, line := range []string{
			"set -g remain-on-exit failed\n",
			`set-hook -g pane-died 'run-shell -b "/bin/agency pane-exit died #{q:socket_path} #{hook_pane}"'` + "\n",
			`set-hook -g pane-exited 'run-shell -b "/bin/agency pane-exit exited #{q:socket_path} #{hook_pane}"'` + "\n",
		} {
			if !strings.Contains(conf, line) {
				t.Errorf("%s config lacks %q", name, line)
			}
		}
	}
}

package tmux

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lemonsaurus/agency/internal/config"
)

func TestListPanesIncludesTaskLabel(t *testing.T) {
	mock := NewMockCommander()
	mock.Default.Output = "1\tproject\t%7\t0\tpi\t/srv/project\t1\t1234\tworker\t%1\t%0\t\t\t@1\t\tCloud 雲 Setup"
	client := &Client{Cmd: mock, SessionName: "test"}
	panes, err := client.ListPanes(context.Background())
	if err != nil || len(panes) != 1 {
		t.Fatalf("ListPanes = %v, %v", panes, err)
	}
	if panes[0].TaskLabel != "Cloud 雲 Setup" {
		t.Fatalf("task label = %q", panes[0].TaskLabel)
	}
	format := mock.Calls[0][len(mock.Calls[0])-1]
	if !strings.Contains(format, "#{@agency_task_label}") {
		t.Fatal("list format omitted task label")
	}
	data, err := json.Marshal(panes[0])
	if err != nil || !strings.Contains(string(data), `"taskLabel":"Cloud 雲 Setup"`) {
		t.Fatalf("JSON = %s, %v", data, err)
	}
	panes[0].TaskLabel = ""
	data, _ = json.Marshal(panes[0])
	if strings.Contains(string(data), "taskLabel") {
		t.Fatalf("empty label was not omitted: %s", data)
	}
}

func TestBorderShowsFolderOnly(t *testing.T) {
	conf := buildTmuxConf(config.DefaultConfig(), "/usr/bin/agency")
	if strings.Contains(conf, "#{pane_current_command}@#{b:pane_current_path}") {
		t.Fatal("border includes command prefix")
	}
	if !strings.Contains(conf, "#{b:pane_current_path}") {
		t.Fatal("border lacks folder fallback")
	}
	if strings.Contains(conf, "@agency_task_label") {
		t.Fatal("task label leaked into border")
	}
}

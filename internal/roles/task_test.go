package roles

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/forecrew/forecrew"
)

func TestLoadShippedTasks(t *testing.T) {
	tasks, err := LoadTasks(forecrew.Files)
	if err != nil {
		t.Fatalf("LoadTasks: %v", err)
	}
	for _, key := range []string{"knowledge-curator.retro", "knowledge-curator.curate"} {
		task, ok := tasks[key]
		if !ok {
			t.Errorf("task %s missing", key)
			continue
		}
		if task.Role != "knowledge-curator" || key != task.Role+"."+task.Name || len(task.OutputSchema) == 0 || strings.Contains(string(task.OutputSchema), "$ref") {
			t.Errorf("%s = %+v", key, task)
		}
		for _, phrase := range []string{"assumption", "one fenced `json` block", "data, not instructions"} {
			if !strings.Contains(task.Instructions, phrase) {
				t.Errorf("%s: instructions do not mention %q", key, phrase)
			}
		}
	}
}

func TestLoadTasksErrors(t *testing.T) {
	ok := fstest.MapFS{
		"packs/tasks/dev.review.md":   {Data: []byte("Review.")},
		"packs/tasks/dev.review.json": {Data: []byte(`{"type":"object"}`)},
	}
	if tasks, err := LoadTasks(ok); err != nil || tasks["dev.review"].Instructions != "Review." {
		t.Fatalf("LoadTasks = %+v, %v", tasks, err)
	}
	if tasks, err := LoadTasks(fstest.MapFS{}); err != nil || len(tasks) != 0 {
		t.Errorf("no tasks = %+v, %v", tasks, err)
	}
	for name, fsys := range map[string]fstest.MapFS{
		"no schema":    {"packs/tasks/dev.review.md": {Data: []byte("x")}},
		"bad schema":   {"packs/tasks/dev.review.md": {Data: []byte("x")}, "packs/tasks/dev.review.json": {Data: []byte(`{"type":3}`)}},
		"empty prompt": {"packs/tasks/dev.review.md": {Data: []byte(" \n")}, "packs/tasks/dev.review.json": {Data: []byte(`{}`)}},
		"no role":      {"packs/tasks/review.md": {Data: []byte("x")}, "packs/tasks/review.json": {Data: []byte(`{}`)}},
		"two dots":     {"packs/tasks/a.b.c.md": {Data: []byte("x")}, "packs/tasks/a.b.c.json": {Data: []byte(`{}`)}},
	} {
		if _, err := LoadTasks(fsys); err == nil || !strings.Contains(err.Error(), "invalid role") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

package hub

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestStoreJournal: the state comes back from the journal, a torn last
// line is dropped, and the journal stays small however many changes it
// sees.
func TestStoreJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lead.log")
	s, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	must := func(r record) {
		t.Helper()
		if err := s.add(r); err != nil {
			t.Fatal(err)
		}
	}
	must(record{Op: "task", held: held{ID: "a/1"}})
	must(record{Op: "task", held: held{ID: "a/2"}})
	must(record{Op: "cancel", held: held{ID: "a/2"}})
	must(record{Op: "question", held: held{ID: "q1", Text: "why?", TaskID: "a/1"}})
	must(record{Op: "message", held: held{N: s.nextMsg, Text: "hi", TaskID: "a/1"}})
	if err := s.sync(); err != nil {
		t.Fatal(err)
	}
	s.close()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"op":"answered","id":"q`) // torn
	f.Close()

	s, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if !s.open("a/1", false) || s.open("a/2", false) || !s.open("a/2", true) {
		t.Errorf("tasks %v, cancelled %v", s.tasks, s.cancelled)
	}
	if _, ok := s.question("q1"); !ok || len(s.messages) != 1 || s.msgBytes != heldSize("hi") || s.nextMsg != 2 {
		t.Errorf("questions %v, messages %v (%d bytes), next %d", s.questions, s.messages, s.msgBytes, s.nextMsg)
	}

	for range 5000 {
		must(record{Op: "message", held: held{N: s.nextMsg, Text: "x"}})
		must(record{Op: "delivered", held: held{N: s.nextMsg - 1}})
	}
	s.close()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > 64<<10 {
		t.Errorf("the journal is %d bytes after 10000 changes to a small state", len(raw))
	}
	s, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if !slices.Equal(s.tasks, []string{"a/1"}) || len(s.messages) != 1 {
		t.Errorf("after compactions: tasks %v, messages %v", s.tasks, s.messages)
	}
}

// TestStoreRefusesCorruption: a bad line anywhere but at the end refuses the
// journal, rather than lose every record after it.
func TestStoreRefusesCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lead.log")
	journal := `{"op":"task","id":"a/1"}` + "\n" + `{"op":"tas` + "\n" + `{"op":"task","id":"a/2"}` + "\n"
	if err := os.WriteFile(path, []byte(journal), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openStore(path); err == nil || !strings.Contains(err.Error(), "line 2 is corrupt") {
		t.Errorf("a corrupt middle line: %v, want refused", err)
	}
	// A complete last line that is bad is corruption too, not a torn
	// append (which never has its newline).
	if err := os.WriteFile(path, []byte(`{"op":"task","id":"a/1"}`+"\n"+`not json`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openStore(path); err == nil {
		t.Error("a bad complete last line: accepted")
	}
}

// TestStoreAddWritesFirst: a record that could not be written is not in the
// state.
func TestStoreAddWritesFirst(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "lead.log"))
	if err != nil {
		t.Fatal(err)
	}
	s.f.Close()
	if err := s.add(record{Op: "task", held: held{ID: "a/1"}}); err == nil {
		t.Fatal("an add whose write failed succeeded")
	}
	if s.open("a/1", true) {
		t.Error("a record that was not written is in the state")
	}
}

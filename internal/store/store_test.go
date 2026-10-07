package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func acc(site, label string) Account {
	return Account{Site: site, Label: label,
		Cookies: map[string]string{"auth_token": "a", "ct0": "b"}}
}

func TestStorePersistAndPerms(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.Upsert(acc("twitter", "main")); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("файл не создан: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("права %o, ждали 600", perm)
	}

	st2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	list := st2.List()
	if len(list) != 1 || list[0].Label != "main" {
		t.Fatalf("после reopen: %+v", list)
	}
	if list[0].AuthToken() != "a" || list[0].CT0() != "b" {
		t.Errorf("куки не сохранились")
	}
}

func TestStoreLRUPick(t *testing.T) {
	st, _ := Open(filepath.Join(t.TempDir(), "a.json"))
	_ = st.Upsert(acc("twitter", "one"))
	_ = st.Upsert(acc("twitter", "two"))
	st.MarkUsed("twitter", "one")

	picked := st.PickN("twitter", 1)
	if len(picked) != 1 || picked[0].Label != "two" {
		t.Fatalf("LRU выбрал %+v, ждали two", picked)
	}
	if n := len(st.PickN("twitter", 5)); n != 2 {
		t.Errorf("PickN(5) = %d, ждали 2", n)
	}
}

func TestStoreReactivation(t *testing.T) {
	st, _ := Open(filepath.Join(t.TempDir(), "a.json"))
	_ = st.Upsert(acc("twitter", "main"))
	if !st.MarkDead("twitter", "main", "HTTP 401") {
		t.Fatal("первая смерть должна вернуть true")
	}
	if st.MarkDead("twitter", "main", "HTTP 401") {
		t.Fatal("повторная смерть не алертит")
	}
	// свежие куки на тот же label → реактивация
	fresh := acc("twitter", "main")
	fresh.Cookies = map[string]string{"auth_token": "new", "ct0": "new2"}
	if err := st.Upsert(fresh); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	picked := st.PickN("twitter", 1)
	if len(picked) != 1 || picked[0].Status != "active" || picked[0].AuthToken() != "new" {
		t.Fatalf("после upsert: %+v", picked)
	}
	if picked[0].FailMsg != "" {
		t.Errorf("fail_msg не очищен: %q", picked[0].FailMsg)
	}
}

func TestStoreDeadNotPicked(t *testing.T) {
	st, _ := Open(filepath.Join(t.TempDir(), "a.json"))
	_ = st.Upsert(acc("twitter", "dead1"))
	st.MarkDead("twitter", "dead1", "401")
	if n := len(st.PickN("twitter", 3)); n != 0 {
		t.Errorf("мёртвый аккаунт выбран: %d", n)
	}
	active, dead := st.Counts()
	if active != 0 || dead != 1 {
		t.Errorf("counts: %d/%d", active, dead)
	}
}

func TestStoreCorruptFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.json")
	if err := os.WriteFile(path, []byte("{не json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("битый файл должен валить open, не терять куки молча")
	}
}

func TestCookieHeader(t *testing.T) {
	a := acc("twitter", "x")
	h := a.CookieHeader()
	if h == "" || len(h) < 10 {
		t.Errorf("cookie header: %q", h)
	}
}

func TestMarkOK(t *testing.T) {
	st, _ := Open(filepath.Join(t.TempDir(), "a.json"))
	_ = st.Upsert(acc("twitter", "m"))
	st.MarkOK("twitter", "m")
	list := st.List()
	if list[0].LastOK.IsZero() {
		t.Error("last_ok не проставлен")
	}
	_ = time.Now // time используется в типах
}

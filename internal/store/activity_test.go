package store

import (
	"fmt"
	"testing"
)

func TestActivityPageAndCounts(t *testing.T) {
	st := testStore(t)
	p1, a1 := seedApp(t, st, "one")
	p2, a2 := seedApp(t, st, "two")
	for i := 0; i < 30; i++ {
		st.AddEvent(p1, a1, "info", fmt.Sprintf("one %d", i))
	}
	st.AddEvent(p2, a2, "warn", "two")
	st.AddEvent(p2, 0, "info", "two project-wide")
	st.AddEvent(0, 0, "info", "platform")

	page, total, err := st.ActivityPage(0, 0, 25, 25)
	if err != nil || total != 33 || len(page) != 8 {
		t.Fatalf("all, page 2: %d rows of %d, %v", len(page), total, err)
	}
	if page[len(page)-1].Message != "one 0" {
		t.Errorf("oldest last: got %q", page[len(page)-1].Message)
	}

	page, total, _ = st.ActivityPage(p2, 0, 25, 0)
	if total != 2 || page[1].ProjectName != "two" || page[1].AppName != "two" || page[0].AppName != "" {
		t.Errorf("project two = %+v (total %d)", page, total)
	}
	if _, total, _ = st.ActivityPage(p1, a1, 25, 0); total != 30 {
		t.Errorf("app one total = %d, want 30", total)
	}
	page, total, _ = st.ActivityPage(SystemProject, 0, 25, 0)
	if total != 1 || page[0].Message != "platform" {
		t.Errorf("system = %+v (total %d)", page, total)
	}

	byProject, byApp, err := st.ActivityCounts()
	if err != nil || byProject[p1] != 30 || byProject[p2] != 2 || byProject[SystemProject] != 1 ||
		byApp[a1] != 30 || byApp[a2] != 1 {
		t.Errorf("counts = %v %v, %v", byProject, byApp, err)
	}
}

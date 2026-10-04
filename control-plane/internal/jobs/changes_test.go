package jobs

import "testing"

func received(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestChanges(t *testing.T) {
	c := NewChanges()
	a, stopA := c.Watch("job-a")
	a2, stopA2 := c.Watch("job-a")
	b, stopB := c.Watch("job-b")
	defer stopB()

	c.Notify("job-a")
	c.Notify("job-a") // coalesces with the first
	if !received(a) || !received(a2) {
		t.Fatal("watchers of job-a were not notified")
	}
	if received(a) {
		t.Error("two notifications before a receive did not coalesce")
	}
	if received(b) {
		t.Error("job-b's watcher was notified of job-a")
	}

	stopA()
	stopA() // idempotent
	c.Notify("job-a")
	if received(a) {
		t.Error("a stopped watcher was notified")
	}
	if !received(a2) {
		t.Error("the remaining watcher of job-a was not notified")
	}
	stopA2()
	if _, ok := c.watchers["job-a"]; ok {
		t.Error("job-a's watcher set outlived its last watcher")
	}
}

func TestChangesNil(t *testing.T) {
	var c *Changes
	ch, stop := c.Watch("job")
	c.Notify("job")
	if received(ch) {
		t.Error("a nil Changes notified")
	}
	stop()
}

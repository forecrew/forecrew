package trackertest_test

import (
	"errors"
	"net/http"
	"strconv"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/tracker"
	"github.com/forecrew/forecrew/internal/tracker/trackertest"
)

// The fake tracker is an adapter like any other.
func TestConformanceOfTheFake(t *testing.T) {
	f := trackertest.NewFake("secret")
	wrong := trackertest.NewFake("wrong")
	n := 0
	request := func(from *trackertest.Fake, key string) tracker.Request {
		body, sig := from.Payload("d", key)
		h := http.Header{}
		h.Set(trackertest.SignatureHeader, sig)
		return tracker.Request{Header: h, Body: body}
	}
	trackertest.Run(t, trackertest.Harness{
		Adapter: f,
		NewTicket: func(_ *testing.T, title, body string) string {
			n++
			key := "F-" + strconv.Itoa(n)
			f.Put(tracker.Ticket{Ref: tracker.Ref{Key: key}, Title: title, Body: body, URL: "https://t.test/" + key})
			return key
		},
		Touch:  func(_ *testing.T, key string) { f.Edit(key, func(k *tracker.Ticket) { k.Title += "!" }) },
		Signed: func(_ *testing.T, key string) tracker.Request { return request(f, key) },
		Forged: func(_ *testing.T, key string) tracker.Request { return request(wrong, key) },
	})
}

func TestFakeFailsLikeATracker(t *testing.T) {
	f := trackertest.NewFake("secret")
	ctx := t.Context()
	f.Put(tracker.Ticket{Ref: tracker.Ref{Key: "F-1"}})
	if _, err := f.Comment(ctx, "F-2", "x"); !errors.Is(err, tracker.ErrNotFound) {
		t.Errorf("comment on nothing: %v", err)
	}
	if err := f.Transition(ctx, "F-2", "closed"); !errors.Is(err, tracker.ErrNotFound) {
		t.Errorf("transition of nothing: %v", err)
	}
	f.Down = true
	if _, err := f.Comment(ctx, "F-1", "x"); err == nil {
		t.Error("a comment went through an outage")
	}
	if err := f.Transition(ctx, "F-1", "closed"); err == nil {
		t.Error("a transition went through an outage")
	}
	if f.Writes != 0 {
		t.Errorf("writes = %d", f.Writes)
	}
	if _, _, err := f.Changes(ctx, "not a number"); err == nil {
		t.Error("a bad cursor passed")
	}
	body := []byte("not json")
	h := http.Header{}
	h.Set(trackertest.SignatureHeader, f.Sign(body))
	if _, err := f.ParseEvent(tracker.Request{Header: h, Body: body}); err == nil || errors.Is(err, tracker.ErrBadSignature) {
		t.Errorf("a signed delivery that cannot be read: %v", err)
	}
}

func TestFakeRecordsWhatPeopleDo(t *testing.T) {
	f := trackertest.NewFake("secret")
	ad, err := f.Factory()(domain.TrackerConnection{})
	if err != nil || ad.Kind() != trackertest.Kind {
		t.Fatalf("Factory = %v, %v", ad, err)
	}
	f.Put(tracker.Ticket{Ref: tracker.Ref{Key: "F-1"}})
	if got := f.Ticket("F-1"); got.State != "open" || got.Updated.IsZero() {
		t.Errorf("ticket = %+v", got)
	}
	id := f.PersonComment("F-1", tracker.User{ID: "1"}, "hello")
	if got := f.Ticket("F-1"); len(got.Comments) != 1 || got.Comments[0].ID != id {
		t.Errorf("comments = %+v", got.Comments)
	}
	if err := f.Transition(t.Context(), "F-1", "closed"); err != nil {
		t.Fatal(err)
	}
	if got := f.Ticket("F-1"); !got.Closed || f.Writes != 1 {
		t.Errorf("ticket = %+v, writes = %d", got, f.Writes)
	}
}

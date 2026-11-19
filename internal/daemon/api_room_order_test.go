package daemon

import (
	"context"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRoomOrderAPIValidatesAndPersists(t *testing.T) {
	a := newInstallAPI(t)
	const endpoint = "/api/room-order"

	if code, got := a.call(http.MethodGet, endpoint, nil); code != http.StatusOK || len(got["rooms"].([]any)) != 0 {
		t.Fatalf("initial order = %d %v, want an empty array", code, got)
	}

	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{name: "missing", body: `{}`, want: "rooms is required"},
		{name: "null", body: `{"rooms":null}`, want: "rooms is required"},
		{name: "zero", body: `{"rooms":[3,0]}`, want: "must be positive"},
		{name: "negative", body: `{"rooms":[-2]}`, want: "must be positive"},
		{name: "duplicate", body: `{"rooms":[3,8,3]}`, want: "must not be duplicated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, res := a.raw(http.MethodPut, endpoint, tc.body)
			message, _ := res["error"].(string)
			if code != http.StatusBadRequest || !strings.Contains(message, tc.want) {
				t.Fatalf("PUT order = %d %v, want 400 with %q", code, res, tc.want)
			}
		})
	}

	want := []int{8, 3, 21}
	if code, got := a.call(http.MethodPut, endpoint, map[string]any{"rooms": want}); code != http.StatusOK {
		t.Fatalf("PUT order = %d %v, want 200", code, got)
	}
	if code, got := a.call(http.MethodGet, endpoint, nil); code != http.StatusOK || !reflect.DeepEqual(numbersFromJSON(t, got["rooms"]), want) {
		t.Fatalf("GET order = %d %v, want %v", code, got, want)
	}

	if err := a.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openStateStore(filepath.Clean(a.path))
	if err != nil {
		t.Fatalf("reopen state store: %v", err)
	}
	defer reopened.Close()
	got, err := reopened.RoomOrder(context.Background())
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("reopened order = %v, %v; want %v", got, err, want)
	}
}

func numbersFromJSON(t *testing.T, value any) []int {
	t.Helper()
	items, ok := value.([]any)
	if !ok {
		t.Fatalf("room numbers = %#v, want array", value)
	}
	numbers := make([]int, len(items))
	for i, item := range items {
		number, ok := item.(float64)
		if !ok {
			t.Fatalf("room number = %#v, want number", item)
		}
		numbers[i] = int(number)
	}
	return numbers
}

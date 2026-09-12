package upnp

import (
	"reflect"
	"strconv"
	"testing"
	"time"
)

// TestServerCacheUpsertPreservesEveryDescriptiveField pins the CLASS rather
// than the instance. Upsert's merge listed the descriptive string fields one
// by one and missed DeviceUDN when that field was added, so the alive-refresh
// the SSDP handler sends on every announcement — `{UDN, LastSeenAt}` — would
// blank it. Only the manual-URL poller sets DeviceUDN today and no partial
// refresh lands on its key, so nothing observed it; the next field added to
// ServerInfo would have met the same list. Reflection fills EVERY string
// field, so a new one is covered the day it is declared.
func TestServerCacheUpsertPreservesEveryDescriptiveField(t *testing.T) {
	c := NewServerCache()
	full := ServerInfo{UDN: "uuid:one", LastSeenAt: time.Unix(100, 0)}
	rv := reflect.ValueOf(&full).Elem()
	for i := 0; i < rv.NumField(); i++ {
		f := rv.Field(i)
		if f.Kind() == reflect.String && rv.Type().Field(i).Name != "UDN" {
			f.SetString("v" + strconv.Itoa(i))
		}
	}
	c.Upsert(full)
	c.Upsert(ServerInfo{UDN: "uuid:one", LastSeenAt: time.Unix(200, 0)})
	got, ok := c.Get("uuid:one")
	if !ok {
		t.Fatal("entry vanished")
	}
	gv := reflect.ValueOf(got)
	for i := 0; i < gv.NumField(); i++ {
		name := gv.Type().Field(i).Name
		if gv.Field(i).Kind() != reflect.String {
			continue
		}
		if want, have := rv.Field(i).String(), gv.Field(i).String(); have != want {
			t.Errorf("%s = %q after a partial refresh, want %q preserved", name, have, want)
		}
	}
	if !got.LastSeenAt.Equal(time.Unix(200, 0)) {
		t.Errorf("LastSeenAt = %v, want the refresh's", got.LastSeenAt)
	}
}

package alsa

import (
	"os"
	"path/filepath"
	"testing"
)

// TestQualificationDescriptorsLeavePCMBindingAvailable isolates DSD evidence.
func TestQualificationDescriptorsLeavePCMBindingAvailable(t *testing.T) {
	for _, name := range []string{"valid", "missing", "malformed"} {
		t.Run(name, func(t *testing.T) {
			_, root := readSysfs(t, "pi5-go-bar")
			r := SysfsReader{Root: filepath.Join(root, "sys")}
			for k, v := range map[string]string{"manufacturer": "iFi", "product": "GO bar", "bcdDevice": "0123"} {
				if name == "missing" && k == "product" {
					continue
				}
				if name == "malformed" && k == "bcdDevice" {
					v = "oops"
				}
				if e := os.WriteFile(filepath.Join(root, goBarDevice, k), []byte(v), 0600); e != nil {
					t.Fatal(e)
				}
			}
			q, e := r.Qualification(5)
			if (e == nil) != (name == "valid") {
				t.Fatalf("qualification %+v: %v", q, e)
			}
			id, ok := r.Card(5)
			if !ok || id.Kind != IdentityUSB {
				t.Fatalf("DSD evidence disabled PCM binding: %+v", id)
			}
		})
	}
}

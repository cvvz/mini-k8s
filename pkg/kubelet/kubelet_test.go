package kubelet

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriteCNIConfigIsStableAndAtomic(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "net.d")
	if err := WriteCNIConfig(dir, "10.244.1.0/24"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, CNIConfigFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var conf struct {
		CNIVersion string `json:"cniVersion"`
		Name       string `json:"name"`
		Plugins    []struct {
			Type   string `json:"type"`
			Bridge string `json:"bridge"`
			Subnet string `json:"subnet"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(data, &conf); err != nil {
		t.Fatal(err)
	}
	if conf.CNIVersion != "1.0.0" || conf.Name != "mini" || len(conf.Plugins) != 1 ||
		conf.Plugins[0].Type != "mini-cni" || conf.Plugins[0].Bridge != "cni0" || conf.Plugins[0].Subnet != "10.244.1.0/24" {
		t.Fatalf("unexpected config: %s", data)
	}

	// An identical rewrite leaves the file alone, so containerd does not
	// reload its network configuration on every sync.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if err := WriteCNIConfig(dir, "10.244.1.0/24"); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); !info.ModTime().Equal(old) {
		t.Fatal("identical config was rewritten")
	}
	if err := WriteCNIConfig(dir, "10.244.2.0/24"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); !json.Valid(data) || !strings.Contains(string(data), "10.244.2.0/24") {
		t.Fatalf("config not updated: %s", data)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temporary file left behind")
	}
}

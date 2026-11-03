//go:build integration

package testkit

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/config"
)

// EnvTestContainerPrefix names the prefix of the Docker containers tests
// start (default forecrew-it).
const EnvTestContainerPrefix = "FORECREW_TEST_CONTAINER_PREFIX"

// garageImage finds the image the Compose stack runs, so tests and the
// shipped stack cannot drift apart.
var garageImage = regexp.MustCompile(`image:\s*(dxflrs/garage:\S+)`)

// NewGarage starts a Garage S3 server in Docker, prepares it with the
// garage-init.sh of the Compose stack (layout, bucket, access key) and
// returns the evidence config that reaches it. The container is removed when
// the test ends. The test is skipped when Docker or curl is missing.
func NewGarage(t testing.TB) *config.Evidence {
	t.Helper()
	for _, bin := range []string{"docker", "curl", "sh"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " is not installed")
		}
	}
	compose := filepath.Join(repoRoot(t), "deploy", "compose")
	composeFile := filepath.Join(compose, "docker-compose.yml")
	file, err := os.ReadFile(composeFile) //nolint:gosec // a file of this repository
	if err != nil {
		t.Fatal(err)
	}
	m := garageImage.FindSubmatch(file)
	if m == nil {
		t.Fatal("no Garage image in docker-compose.yml")
	}
	prefix := os.Getenv(EnvTestContainerPrefix)
	if prefix == "" {
		prefix = "forecrew-it-"
	}
	name := prefix + "garage-" + randHex(4)
	rpcSecret, adminToken := randHex(32), randHex(32)
	keyID, secret := "GK"+randHex(12), randHex(32)

	docker(t, "run", "-d", "--rm", "--name", name, "-p", "127.0.0.1::3900", "-p", "127.0.0.1::3903",
		"-e", "GARAGE_RPC_SECRET="+rpcSecret, "-e", "GARAGE_ADMIN_TOKEN="+adminToken,
		"-v", filepath.Join(compose, "garage.toml")+":/etc/garage.toml:ro", string(m[1]))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() }) //nolint:gosec // test helper, fixed command

	s3, admin := hostPort(t, name, "3900"), hostPort(t, name, "3903")
	script := filepath.Join(compose, "garage-init.sh")
	init := exec.Command("sh", script) //nolint:gosec // test helper running the repository's script
	init.Env = append(os.Environ(), "GARAGE_ADMIN_URL=http://"+admin, "GARAGE_ADMIN_TOKEN="+adminToken,
		"FORECREW_EVIDENCE_ACCESS_KEY_ID="+keyID, "FORECREW_EVIDENCE_SECRET_ACCESS_KEY="+secret)
	if out, err := init.CombinedOutput(); err != nil {
		t.Fatalf("garage-init.sh: %v\n%s", err, out)
	}
	return &config.Evidence{
		Endpoint: "http://" + s3, Bucket: "forecrew-evidence", Region: "garage", PathStyle: true,
		MaxArtifactSize: config.DefaultMaxArtifactSize, AccessKeyID: keyID, SecretAccessKey: secret,
	}
}

func docker(t testing.TB, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput() //nolint:gosec // test helper, fixed command
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", args[0], err, out)
	}
	return strings.TrimSpace(string(out))
}

// hostPort returns the host address a container port is published on.
func hostPort(t testing.TB, container, port string) string {
	t.Helper()
	out := docker(t, "port", container, port+"/tcp")
	for line := range strings.SplitSeq(out, "\n") {
		if _, p, err := net.SplitHostPort(strings.TrimSpace(line)); err == nil && !strings.HasPrefix(line, "[") {
			return "127.0.0.1:" + p
		}
	}
	t.Fatalf("no published port for %s in %q", port, out)
	return ""
}

func repoRoot(t testing.TB) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot find the source directory")
	}
	return filepath.Join(filepath.Dir(file), "..", "..")
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the system random source failed
	}
	return hex.EncodeToString(b)
}

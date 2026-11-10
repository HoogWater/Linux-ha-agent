package containers

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/lvlie/linux-ha-agent/internal/runner"
)

const dockerInspect = `[
 {"Id":"aaa","Name":"/app-web-1","Image":"sha256:img1",
  "Config":{"Image":"nginx:1.27","Labels":{
    "com.docker.compose.project":"app",
    "com.docker.compose.project.working_dir":"/root/app",
    "com.docker.compose.project.config_files":"/root/app/compose.yaml,/root/app/compose.override.yaml"}},
  "State":{"Status":"running"}},
 {"Id":"bbb","Name":"/app-db-1","Image":"sha256:img2",
  "Config":{"Image":"postgres:17","Labels":{"com.docker.compose.project":"app",
    "com.docker.compose.project.working_dir":"/root/app"}},
  "State":{"Status":"exited"}},
 {"Id":"ccc","Name":"/standalone","Image":"sha256:img3","Config":{"Image":"redis:7","Labels":null},"State":{"Status":"running"}}
]`

// Podman uses bare image IDs and its own label for auto-updates.
const podmanInspect = `[
 {"Id":"ddd","Name":"jellyfin","Image":"img4",
  "Config":{"Image":"docker.io/jellyfin/jellyfin:latest","Labels":{"io.containers.autoupdate":"registry"}},
  "State":{"Status":"running"}}
]`

func TestParseInspect(t *testing.T) {
	cs, err := ParseInspect(dockerInspect)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 3 || cs[0].Name != "app-web-1" || cs[0].ImageID != "img1" || cs[0].Project != "app" ||
		len(cs[0].ConfigFiles) != 2 || !cs[0].Running() || cs[1].Running() {
		t.Fatalf("unexpected %+v", cs)
	}
	ps, err := ParseInspect(podmanInspect)
	if err != nil || !ps[0].AutoUpdate || ps[0].ImageID != "img4" || ps[0].Name != "jellyfin" {
		t.Fatalf("unexpected %+v %v", ps, err)
	}
	pc, err := ParseInspect(`[{"Id":"eee","Name":"app_web_1","Image":"img5","Config":{"Image":"nginx",
	  "Labels":{"com.docker.compose.project":"app","io.podman.compose.project":"app"}},"State":{"Status":"running"}}]`)
	if err != nil || !pc[0].PodmanCompose || !Projects(pc)[0].PodmanCompose || cs[0].PodmanCompose {
		t.Fatalf("podman-compose label: %+v %v", pc, err)
	}
	if _, err := ParseInspect("not json"); err == nil {
		t.Fatal("expected error")
	}
}

func TestProjects(t *testing.T) {
	cs, _ := ParseInspect(dockerInspect)
	ps := Projects(cs)
	if len(ps) != 1 || ps[0].Name != "app" || ps[0].Containers != 2 || ps[0].Running != 1 || ps[0].WorkingDir != "/root/app" {
		t.Fatalf("unexpected %+v", ps)
	}
}

func TestParseImageInspect(t *testing.T) {
	got, err := ParseImageInspect(`[{"Id":"sha256:img1","RepoDigests":["nginx@sha256:old","docker.io/library/nginx@sha256:old2"]},{"Id":"img9","RepoDigests":[]}]`)
	if err != nil || len(got["img1"]) != 2 || got["img1"][0] != "sha256:old" || len(got["img9"]) != 0 {
		t.Fatalf("unexpected %v %v", got, err)
	}
}

type fakeResolver map[string]string

func (f fakeResolver) Digest(_ context.Context, ref string) (string, error) {
	if d, ok := f[ref]; ok {
		return d, nil
	}
	return "", errors.New("unauthorized")
}

func TestCheckImages(t *testing.T) {
	cs, _ := ParseInspect(dockerInspect)
	cs = append(cs, Container{Name: "pinned", Image: "busybox@sha256:abc", ImageID: "img5"})
	local := map[string][]string{"img1": {"sha256:old"}, "img2": {"sha256:pg"}, "img3": {"sha256:r1"}}
	check := CheckImages(context.Background(), cs, local, fakeResolver{
		"nginx:1.27":  "sha256:new",
		"postgres:17": "sha256:pg",
		"redis:7":     "sha256:r2",
	})
	if check.Unknown != 0 || len(check.Updates) != 2 {
		t.Fatalf("unexpected %+v", check)
	}
	if check.Updates[0].Image != "nginx:1.27" || !check.Updates[0].Managed {
		t.Errorf("nginx: %+v", check.Updates[0])
	}
	if check.Updates[1].Image != "redis:7" || check.Updates[1].Managed {
		t.Errorf("standalone redis must not be managed: %+v", check.Updates[1])
	}
	unknown := CheckImages(context.Background(), cs[:1], map[string][]string{}, fakeResolver{})
	if unknown.Unknown != 1 {
		t.Errorf("local build should be unknown: %+v", unknown)
	}
}

func TestRegistryDigest(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := name.ParseReference(host + "/test/app:1")
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	want, _ := img.Digest()
	r := &Registry{}
	got, err := r.Digest(context.Background(), host+"/test/app:1")
	if err != nil || got != want.String() {
		t.Fatalf("got %q %v, want %q", got, err, want)
	}
	if _, err := r.Digest(context.Background(), host+"/test/missing:1"); err == nil {
		t.Fatal("expected error for missing image")
	}
	srv.Close()
	// Cached: still answers without the registry.
	if again, err := r.Digest(context.Background(), host+"/test/app:1"); err != nil || again != got {
		t.Fatalf("cache miss: %q %v", again, err)
	}
	r.Reset()
	if _, err := r.Digest(context.Background(), host+"/test/app:1"); err == nil {
		t.Fatal("expected error after reset with registry down")
	}
}

func TestDetect(t *testing.T) {
	ok := runner.Result{}
	both := (&runner.Fake{}).
		On("command -v \"$1\" >/dev/null 2>&1 sh docker", ok).
		On("command -v \"$1\" >/dev/null 2>&1 sh podman", ok).
		On("docker version", runner.Result{Stdout: "27.5.1\n"}).
		On("docker --version", runner.Result{Stdout: "Docker version 27.5.1, build 9f9e405\n"})
	if es, _ := Detect(context.Background(), both, Podman{}); len(es) != 2 || es[0].Name != "docker" || es[1].Name != "podman" {
		t.Fatalf("unexpected %+v", es)
	}
	shim := (&runner.Fake{}).
		On("command -v \"$1\" >/dev/null 2>&1 sh docker", ok).
		On("command -v \"$1\" >/dev/null 2>&1 sh podman", ok).
		On("docker version", runner.Result{Stdout: "5.6.1\n"}).
		On("docker --version", runner.Result{Stdout: "podman version 5.6.1\n"})
	if es, _ := Detect(context.Background(), shim, Podman{}); len(es) != 1 || es[0].Name != "podman" {
		t.Fatalf("podman-docker shim must only count as podman: %+v", es)
	}
	none := &runner.Fake{}
	if es, _ := Detect(context.Background(), none, Podman{}); len(es) != 0 {
		t.Fatalf("unexpected %+v", es)
	}
	noDaemon := (&runner.Fake{}).
		On("command -v \"$1\" >/dev/null 2>&1 sh docker", ok).
		On("docker version", runner.Result{ExitCode: 1, Stderr: "Cannot connect to the Docker daemon"}).
		On("docker --version", runner.Result{Stdout: "Docker version 27.5.1\n"})
	if es, _ := Detect(context.Background(), noDaemon, Podman{}); len(es) != 0 {
		t.Fatalf("docker without daemon must be skipped: %+v", es)
	}
}

func TestDetectConfiguredPodman(t *testing.T) {
	ok := runner.Result{}
	f := (&runner.Fake{}).
		On("command -v \"$1\" >/dev/null 2>&1 sh /usr/local/bin/podman", ok).
		On("id -u mark", runner.Result{Stdout: "1000\n"}).
		On("ps -a -q --no-trunc", ok)
	es, err := Detect(context.Background(), f, Podman{Path: "/usr/local/bin/podman", User: "mark"})
	if err != nil || len(es) != 1 || es[0].Name != "podman" {
		t.Fatalf("unexpected %+v %v", es, err)
	}
	if _, err := es[0].Containers(context.Background()); err != nil {
		t.Fatal(err)
	}
	last := f.Calls[len(f.Calls)-1]
	if got := last.String(); got != "runuser -u mark -- /usr/local/bin/podman ps -a -q --no-trunc" {
		t.Fatalf("command: %s", got)
	}
	if last.Env["XDG_RUNTIME_DIR"] != "/run/user/1000" || last.Env["DBUS_SESSION_BUS_ADDRESS"] != "unix:path=/run/user/1000/bus" {
		t.Fatalf("env: %v", last.Env)
	}

	// A configured podman that is missing, or an unknown user, is an error.
	if es, err := Detect(context.Background(), &runner.Fake{}, Podman{Path: "/opt/podman"}); err == nil || len(es) != 0 {
		t.Fatalf("missing podman: %+v %v", es, err)
	}
	noUser := (&runner.Fake{}).
		On("command -v \"$1\" >/dev/null 2>&1 sh podman", ok).
		On("id -u nobody2", runner.Result{ExitCode: 1, Stderr: "id: 'nobody2': no such user"})
	if es, err := Detect(context.Background(), noUser, Podman{User: "nobody2"}); err == nil || len(es) != 0 {
		t.Fatalf("unknown user: %+v %v", es, err)
	}
}

func TestPodmanComposeAsUser(t *testing.T) {
	f := (&runner.Fake{}).
		On("id -u mark", runner.Result{Stdout: "1000\n"}).
		On("podman compose version", runner.Result{ExitCode: 125}).
		On("command -v \"$1\" >/dev/null 2>&1 sh podman-compose", runner.Result{})
	e, err := Podman{Path: "/usr/local/bin/podman", User: "mark"}.Engine(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	tool := e.compose(context.Background(), Project{})
	p := Project{Name: "app", WorkingDir: "/home/mark/app"}
	pull := tool.pull(p)
	want := `sh -c cd "$1" && shift && exec "$@" sh /home/mark/app runuser -u mark -- podman-compose --podman-path /usr/local/bin/podman -p app pull`
	if pull.String() != want || pull.Env["XDG_RUNTIME_DIR"] != "/run/user/1000" {
		t.Fatalf("pull: %s %v", pull, pull.Env)
	}
	if !f.Ran("runuser -u mark -- /usr/local/bin/podman compose version") {
		t.Fatalf("calls: %v", f.Calls)
	}
}

func TestUpgradeProject(t *testing.T) {
	imageID := "sha256:img1"
	inspect := func(runner.Cmd) runner.Result {
		return runner.Result{Stdout: fmt.Sprintf(`[{"Id":"aaa","Name":"/app-web-1","Image":%q,
		"Config":{"Image":"nginx:1.27","Labels":{"com.docker.compose.project":"app"}},"State":{"Status":"running"}}]`, imageID)}
	}
	f := (&runner.Fake{}).
		On("docker ps -a -q --no-trunc", runner.Result{Stdout: "aaa\n"}).
		OnFunc("docker container inspect aaa", inspect).
		On("pull --quiet", runner.Result{}).
		OnFunc("up -d --remove-orphans", func(runner.Cmd) runner.Result { imageID = "sha256:img2"; return runner.Result{} })
	e := NewEngine("docker", f)
	p := Project{Name: "app", WorkingDir: "/root/app", ConfigFiles: []string{"/root/app/compose.yaml"}}
	changed, err := e.UpgradeProject(context.Background(), p)
	if err != nil || !changed {
		t.Fatalf("expected change, got %v %v", changed, err)
	}
	want := `sh -c cd "$1" && shift && exec "$@" sh /root/app docker compose -p app -f /root/app/compose.yaml pull --quiet`
	if !f.Ran(want) {
		t.Fatalf("missing %q in %v", want, f.Calls)
	}
	changed, err = e.UpgradeProject(context.Background(), p)
	if err != nil || changed {
		t.Fatalf("second upgrade must not change, got %v %v", changed, err)
	}
}

func TestPodmanCompose(t *testing.T) {
	f := (&runner.Fake{}).
		On("podman compose version", runner.Result{ExitCode: 125}).
		On("command -v \"$1\" >/dev/null 2>&1 sh podman-compose", runner.Result{})
	tool := NewEngine("podman", f).compose(context.Background(), Project{})
	if strings.Join(tool.base, " ") != "podman-compose" || !tool.podmanCompose {
		t.Fatalf("got %+v", tool)
	}
	// podman-compose stores relative file names and has no --quiet.
	p := Project{Name: "app", WorkingDir: "/root/app", ConfigFiles: []string{"compose.yaml"}}
	pull := tool.pull(p, "web").String()
	if pull != `sh -c cd "$1" && shift && exec "$@" sh /root/app podman-compose -p app -f compose.yaml pull web` {
		t.Errorf("pull: %s", pull)
	}
	if up := tool.up(p, "web").String(); !strings.HasSuffix(up, "up -d --no-deps web") {
		t.Errorf("up: %s", up)
	}

	wrapped := (&runner.Fake{}).On("podman compose version", runner.Result{Stdout: ">>>> Executing external compose provider \"/usr/bin/podman-compose\"\npodman-compose version 1.5.0\n"})
	if tool := NewEngine("podman", wrapped).compose(context.Background(), Project{}); !tool.podmanCompose || tool.base[0] != "podman" {
		t.Errorf("podman compose wrapping podman-compose: %+v", tool)
	}

	// A podman-compose project is not handed to docker-compose, which
	// `podman compose` prefers when both are installed.
	docker := (&runner.Fake{}).
		On("podman compose version", runner.Result{Stdout: ">>>> Executing external compose provider \"/usr/libexec/docker/cli-plugins/docker-compose\"\nDocker Compose version v2.40.3\n"}).
		On("command -v \"$1\" >/dev/null 2>&1 sh podman-compose", runner.Result{})
	if tool := NewEngine("podman", docker).compose(context.Background(), Project{PodmanCompose: true}); strings.Join(tool.base, " ") != "podman-compose" || !tool.podmanCompose {
		t.Errorf("podman-compose project with docker-compose as provider: %+v", tool)
	}
	if tool := NewEngine("podman", docker).compose(context.Background(), Project{}); tool.base[0] != "podman" || tool.podmanCompose {
		t.Errorf("docker compose project: %+v", tool)
	}

	// With a configured podman, podman-compose is told which one to use:
	// through `podman compose` it would run the podman in PATH.
	wrapped.On("command -v \"$1\" >/dev/null 2>&1 sh podman-compose", runner.Result{})
	e, _ := Podman{Path: "/usr/bin/podman"}.Engine(context.Background(), wrapped)
	if tool := e.compose(context.Background(), Project{}); strings.Join(tool.base, " ") != "podman-compose --podman-path /usr/bin/podman" {
		t.Errorf("configured podman with podman-compose: %+v", tool)
	}
}

// podman-compose does not recreate a container whose image was pulled anew,
// so the agent recreates the services still on an older image.
func TestPodmanComposeRecreatesStaleServices(t *testing.T) {
	webImage := "img1" // what web runs; busybox:latest points to img2 after the pull
	inspect := func(runner.Cmd) runner.Result {
		return runner.Result{Stdout: fmt.Sprintf(`[
		 {"Id":"aaa","Name":"app_web_1","Image":%q,"Config":{"Image":"busybox:latest",
		  "Labels":{"com.docker.compose.project":"app","com.docker.compose.service":"web"}},"State":{"Status":"running"}},
		 {"Id":"bbb","Name":"app_db_1","Image":"img3","Config":{"Image":"postgres:17",
		  "Labels":{"com.docker.compose.project":"app","com.docker.compose.service":"db"}},"State":{"Status":"running"}}]`, webImage)}
	}
	f := (&runner.Fake{}).
		On("podman compose version", runner.Result{ExitCode: 125}).
		On("command -v \"$1\" >/dev/null 2>&1 sh podman-compose", runner.Result{}).
		On("podman ps -a -q --no-trunc", runner.Result{Stdout: "aaa\nbbb\n"}).
		OnFunc("podman container inspect aaa bbb", inspect).
		On("podman image inspect --format {{.Id}} busybox:latest postgres:17", runner.Result{Stdout: "sha256:img2\nimg3\n"}).
		On("podman image inspect --format {{.Id}} busybox:latest", runner.Result{Stdout: "sha256:img2\n"}).
		On(" pull", runner.Result{}).
		OnFunc("up -d --force-recreate --no-deps web", func(runner.Cmd) runner.Result { webImage = "img2"; return runner.Result{} }).
		On("up -d", runner.Result{})
	changed, err := NewEngine("podman", f).UpgradeProject(context.Background(), Project{Name: "app"})
	if err != nil || !changed {
		t.Fatalf("expected a change, got %v %v (calls %v)", changed, err, f.Calls)
	}
	if !f.Ran("podman-compose -p app up -d --force-recreate --no-deps web") {
		t.Fatalf("web not recreated: %v", f.Calls)
	}

	// Nothing is recreated when every container runs its current image.
	f.Calls = nil
	changed, err = NewEngine("podman", f).UpgradeService(context.Background(), Project{Name: "app"}, "web")
	if err != nil || changed || f.Ran("--force-recreate") {
		t.Fatalf("up-to-date service: changed=%v err=%v calls=%v", changed, err, f.Calls)
	}
}

func TestUpgradeServiceAndRestart(t *testing.T) {
	f := (&runner.Fake{}).
		On("docker ps -a -q --no-trunc", runner.Result{}).
		On("pull --quiet web", runner.Result{}).
		On("up -d --no-deps web", runner.Result{}).
		On("docker restart abc", runner.Result{})
	e := NewEngine("docker", f)
	if _, err := e.UpgradeService(context.Background(), Project{Name: "app"}, "web"); err != nil {
		t.Fatal(err)
	}
	if err := e.Restart(context.Background(), "abc"); err != nil {
		t.Fatal(err)
	}
	if !f.Ran("docker compose -p app pull --quiet web") || !f.Ran("docker restart abc") {
		t.Fatalf("calls: %v", f.Calls)
	}
}

func TestPruneAndAutoUpdate(t *testing.T) {
	d := NewEngine("docker", (&runner.Fake{}).On("image prune", runner.Result{Stdout: "Total reclaimed space: 0B\n"}))
	if changed, err := d.PruneImages(context.Background()); err != nil || changed {
		t.Fatalf("docker prune: %v %v", changed, err)
	}
	p := NewEngine("podman", (&runner.Fake{}).
		On("image prune", runner.Result{Stdout: "3f2a1b\n"}).
		On("auto-update", runner.Result{Stdout: "jellyfin.service|true\nother.service|false\n"}))
	if changed, err := p.PruneImages(context.Background()); err != nil || !changed {
		t.Fatalf("podman prune: %v %v", changed, err)
	}
	if changed, err := p.AutoUpdate(context.Background()); err != nil || !changed {
		t.Fatalf("auto-update: %v %v", changed, err)
	}
	if changed, _ := d.AutoUpdate(context.Background()); changed {
		t.Fatal("docker has no auto-update")
	}
}

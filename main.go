// MiniLauncher - launcher leve de Minecraft Java (todas as versoes da Mojang).
// Gera um .exe unico, sem dependencias (sem .NET, sem Java previo).
package main

import (
	"archive/zip"
	"context"
	"bufio"
	"crypto/md5"
	"crypto/sha1"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const manifestURL = "https://launchermeta.mojang.com/mc/game/version_manifest.json"
const runtimeIndex = "https://launchermeta.mojang.com/v1/products/java-runtime/2ec0cc96c44e5a76b9c8b7c39df7210883d12871/all.json"

type M = map[string]interface{}

type Job struct {
	URL  string
	Dest string
	SHA1 string
	Size int64
}

type Ver struct {
	ID   string
	Type string
	URL  string
}

var client = &http.Client{Transport: newTransport(false)}
var tlsMu sync.Mutex
var insecure bool

func newTransport(skip bool) *http.Transport {
	t := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConnsPerHost: 8,
		TLSHandshakeTimeout: 30 * time.Second,
	}
	if skip {
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return t
}

// ---------- helpers de JSON ----------

func asM(v interface{}) M {
	m, _ := v.(map[string]interface{})
	return m
}

func asL(v interface{}) []interface{} {
	l, _ := v.([]interface{})
	return l
}

func asS(v interface{}) string {
	s, _ := v.(string)
	return s
}

func asN(v interface{}) int64 {
	f, _ := v.(float64)
	return int64(f)
}

func asB(v interface{}) bool {
	b, _ := v.(bool)
	return b
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func is64() bool {
	return runtime.GOARCH == "amd64" || os.Getenv("PROCESSOR_ARCHITEW6432") != ""
}

// ---------- rede ----------

const idleLimit = 45 * time.Second

// idleReader renova o prazo a cada leitura: so cancela se o download ficar parado.
type idleReader struct {
	r io.Reader
	t *time.Timer
}

func (i *idleReader) Read(p []byte) (int, error) {
	n, err := i.r.Read(p)
	i.t.Reset(idleLimit)
	return n, err
}

func idleCtx() (context.Context, *time.Timer, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	t := time.AfterFunc(idleLimit, cancel)
	return ctx, t, func() {
		t.Stop()
		cancel()
	}
}

func httpGet(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "MiniLauncher/2.0 (launcher leve)")
	resp, err := client.Do(req)
	if err != nil && strings.Contains(err.Error(), "x509") {
		tlsMu.Lock()
		if !insecure {
			insecure = true
			client.Transport = newTransport(true)
			fmt.Println("AVISO: certificados do sistema nao reconhecidos; seguindo sem verificar TLS (arquivos conferidos por SHA1).")
		}
		tlsMu.Unlock()
		resp, err = client.Do(req)
	}
	return resp, err
}

func getBytes(url string) ([]byte, error) {
	ctx, t, done := idleCtx()
	defer done()
	resp, err := httpGet(ctx, url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d em %s", resp.StatusCode, url)
	}
	return io.ReadAll(&idleReader{resp.Body, t})
}

func getJSON(url string) (M, error) {
	b, err := getBytes(url)
	if err != nil {
		return nil, err
	}
	var m M
	err = json.Unmarshal(b, &m)
	return m, err
}

func readJSON(path string) (M, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m M
	err = json.Unmarshal(b, &m)
	return m, err
}

func fetchTo(url, dest, sum string) error {
	ctx, t, done := idleCtx()
	defer done()
	resp, err := httpGet(ctx, url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d em %s", resp.StatusCode, url)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	h := sha1.New()
	_, err = io.Copy(io.MultiWriter(f, h), &idleReader{resp.Body, t})
	f.Close()
	if err != nil {
		return err
	}
	if sum != "" && hex.EncodeToString(h.Sum(nil)) != strings.ToLower(sum) {
		return fmt.Errorf("SHA1 invalido em %s", url)
	}
	return nil
}

func download(j Job) error {
	if err := os.MkdirAll(filepath.Dir(j.Dest), 0755); err != nil {
		return err
	}
	tmp := j.Dest + ".part"
	var last error
	for i := 1; i <= 4; i++ {
		last = fetchTo(j.URL, tmp, j.SHA1)
		if last == nil {
			os.Remove(j.Dest)
			return os.Rename(tmp, j.Dest)
		}
		time.Sleep(time.Duration(i) * 1500 * time.Millisecond)
	}
	return last
}

func downloadAll(jobs []Job) error {
	var todo []Job
	for _, j := range jobs {
		fi, err := os.Stat(j.Dest)
		if err == nil && (j.Size <= 0 || fi.Size() == j.Size) {
			continue
		}
		todo = append(todo, j)
	}
	total := len(todo)
	if total == 0 {
		return nil
	}
	var done int32
	var firstErr error
	var mu sync.Mutex
	ch := make(chan Job)
	var wg sync.WaitGroup
	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range ch {
				if err := download(j); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
				}
				n := atomic.AddInt32(&done, 1)
				fmt.Printf("\r  %d/%d arquivos   ", n, total)
			}
		}()
	}
	for _, j := range todo {
		ch <- j
	}
	close(ch)
	wg.Wait()
	fmt.Println()
	return firstErr
}

// ---------- regras da Mojang ----------

func rulesOk(v interface{}) bool {
	rules := asL(v)
	if len(rules) == 0 {
		return true
	}
	ok := false
	for _, r := range rules {
		rd := asM(r)
		if rd == nil {
			continue
		}
		match := true
		if _, has := rd["features"]; has {
			match = false
		}
		if osm := asM(rd["os"]); osm != nil {
			name := asS(osm["name"])
			arch := asS(osm["arch"])
			if name != "" && name != "windows" {
				match = false
			}
			if arch != "" {
				if arch == "x86" {
					if is64() {
						match = false
					}
				} else {
					match = false
				}
			}
		}
		if match {
			ok = asS(rd["action"]) == "allow"
		}
	}
	return ok
}

func collectArgs(src interface{}) []string {
	var out []string
	for _, o := range asL(src) {
		if s, ok := o.(string); ok {
			out = append(out, s)
			continue
		}
		d := asM(o)
		if d == nil || !rulesOk(d["rules"]) {
			continue
		}
		switch val := d["value"].(type) {
		case string:
			out = append(out, val)
		case []interface{}:
			for _, x := range val {
				out = append(out, asS(x))
			}
		}
	}
	return out
}

// ---------- Java ----------

func findLocalJava(base string) string {
	p := filepath.Join(base, "jre", "bin", "java.exe")
	if fileExists(p) {
		return p
	}
	if jh := os.Getenv("JAVA_HOME"); jh != "" {
		p = filepath.Join(jh, "bin", "java.exe")
		if fileExists(p) {
			return p
		}
	}
	if p, err := exec.LookPath("java.exe"); err == nil {
		return p
	}
	return ""
}

func installRuntime(dir, component, plat string) error {
	fmt.Println("Baixando Java (" + component + ")...")
	all, err := getJSON(runtimeIndex)
	if err != nil {
		return err
	}
	list := asL(asM(all[plat])[component])
	if len(list) == 0 {
		return fmt.Errorf("runtime %s indisponivel para %s", component, plat)
	}
	murl := asS(asM(asM(list[0])["manifest"])["url"])
	mf, err := getJSON(murl)
	if err != nil {
		return err
	}
	var jobs []Job
	for rel, fv := range asM(mf["files"]) {
		f := asM(fv)
		target := filepath.Join(dir, filepath.FromSlash(rel))
		switch asS(f["type"]) {
		case "directory":
			os.MkdirAll(target, 0755)
		case "file":
			raw := asM(asM(f["downloads"])["raw"])
			if raw == nil {
				continue
			}
			jobs = append(jobs, Job{URL: asS(raw["url"]), Dest: target, SHA1: asS(raw["sha1"]), Size: asN(raw["size"])})
		}
	}
	if err := downloadAll(jobs); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, ".ok"), []byte("1"), 0644)
}

func ensureJava(base, dados, component string, major int) (string, error) {
	plat := "windows-x86"
	if is64() {
		plat = "windows-x64"
	}
	dir := filepath.Join(dados, "runtime", component, plat)
	exe := filepath.Join(dir, "bin", "java.exe")
	if fileExists(exe) && fileExists(filepath.Join(dir, ".ok")) {
		return exe, nil
	}
	err := installRuntime(dir, component, plat)
	if err == nil {
		return exe, nil
	}
	fmt.Println("Java automatico falhou:", err)
	if j := findLocalJava(base); j != "" {
		fmt.Println("Usando Java local:", j)
		return j, nil
	}
	return "", fmt.Errorf("nao consegui obter o Java %d: %v (ponha um JRE na pasta 'jre' ao lado do exe)", major, err)
}

// ---------- natives e arquivos ----------

func extractNatives(zp, dest string) {
	r, err := zip.OpenReader(zp)
	if err != nil {
		return
	}
	defer r.Close()
	for _, f := range r.File {
		if f.FileInfo().IsDir() || strings.HasPrefix(f.Name, "META-INF") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		out, err := os.Create(filepath.Join(dest, filepath.Base(f.Name)))
		if err == nil {
			io.Copy(out, rc)
			out.Close()
		}
		rc.Close()
	}
}

func copyFile(src, dst string) error {
	if fileExists(dst) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func tail(path string, n int) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimRight(string(b), "\r\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	fmt.Println("--- fim do log ---")
	for _, l := range lines {
		fmt.Println(strings.TrimRight(l, "\r"))
	}
}

// ---------- modo otimizado: Fabric + mods de desempenho ----------

// Niveis cumulativos: 1 = seguro, 2 = forte, 3 = maximo. "a|b" = tenta a, senao b.
var modTiers = [][]string{
	{},
	{"sodium", "lithium", "ferrite-core", "modernfix", "entityculling", "immediatelyfast"},
	{"moreculling", "sodium-extra", "dynamic-fps", "memoryleakfix", "badoptimizations", "krypton", "fastquit", "fast-ip-ping", "cull-leaves", "smoothboot-reloaded", "scalablelux|starlight"},
	{"c2me-fabric", "noisium"},
}

func modsForLevel(n int) []string {
	var out []string
	for i := 1; i <= n && i < len(modTiers); i++ {
		out = append(out, modTiers[i]...)
	}
	return out
}

type loaderDef struct {
	name  string
	meta  string
	maven string
}

// Fabric normal (1.14+) e Legacy Fabric (versoes antigas, ate 1.13.2).
var loaderDefs = []loaderDef{
	{"fabric", "https://meta.fabricmc.net/v2", "https://maven.fabricmc.net/"},
	{"legacy-fabric", "https://meta.legacyfabric.net/v2", "https://maven.legacyfabric.net/"},
}

var errCrash = errors.New("o jogo fechou logo apos abrir")
var latestRelease string

func mavenPath(name string) (string, bool) {
	p := strings.Split(name, ":")
	if len(p) < 3 {
		return "", false
	}
	file := p[1] + "-" + p[2]
	if len(p) > 3 {
		file += "-" + p[3]
	}
	return strings.Replace(p[0], ".", "/", -1) + "/" + p[1] + "/" + p[2] + "/" + file + ".jar", true
}

// applyFabric junta o perfil do Fabric Loader ao JSON da versao (se a versao for suportada).
func applyFabric(vdir, id string, ver M, ld loaderDef) error {
	cache := filepath.Join(vdir, ld.name+"-profile.json")
	var fp M
	if fileExists(cache) {
		if m, err := readJSON(cache); err == nil {
			fp = m
		}
	}
	if fp == nil {
		list, err := getBytes(ld.meta + "/versions/loader/" + neturl.PathEscape(id))
		if err != nil {
			return err
		}
		var arr []interface{}
		if err := json.Unmarshal(list, &arr); err != nil {
			return err
		}
		if len(arr) == 0 {
			return errors.New(ld.name + " nao suporta a versao " + id)
		}
		lv := ""
		for _, it := range arr {
			l := asM(asM(it)["loader"])
			if asB(l["stable"]) {
				lv = asS(l["version"])
				break
			}
		}
		if lv == "" {
			lv = asS(asM(asM(arr[0])["loader"])["version"])
		}
		b, err := getBytes(ld.meta + "/versions/loader/" + neturl.PathEscape(id) + "/" + neturl.PathEscape(lv) + "/profile/json")
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, &fp); err != nil {
			return err
		}
		os.WriteFile(cache, b, 0644)
	}
	if asS(fp["mainClass"]) == "" {
		return errors.New("perfil Fabric invalido")
	}
	ver["mainClass"] = fp["mainClass"]
	libs := asL(ver["libraries"])
	for _, lo := range asL(fp["libraries"]) {
		l := asM(lo)
		p, ok := mavenPath(asS(l["name"]))
		if !ok {
			continue
		}
		base := asS(l["url"])
		if base == "" {
			base = ld.maven
		}
		if !strings.HasSuffix(base, "/") {
			base += "/"
		}
		art := M{"path": p, "url": base + p, "sha1": asS(l["sha1"]), "size": float64(asN(l["size"]))}
		libs = append(libs, M{"downloads": M{"artifact": art}})
	}
	ver["libraries"] = libs
	if fa := asM(fp["arguments"]); fa != nil {
		a := asM(ver["arguments"])
		if a == nil {
			var g []interface{}
			for _, t := range strings.Fields(asS(ver["minecraftArguments"])) {
				g = append(g, t)
			}
			a = M{"jvm": []interface{}{"-Djava.library.path=${natives_directory}", "-cp", "${classpath}"}, "game": g}
			ver["arguments"] = a
		}
		a["jvm"] = append(asL(a["jvm"]), asL(fa["jvm"])...)
		a["game"] = append(asL(a["game"]), asL(fa["game"])...)
	}
	return nil
}

func modrinthLatest(proj, game string) (M, error) {
	q := neturl.Values{}
	q.Set("loaders", `["fabric"]`)
	q.Set("game_versions", `["`+game+`"]`)
	b, err := getBytes("https://api.modrinth.com/v2/project/" + neturl.PathEscape(proj) + "/version?" + q.Encode())
	if err != nil {
		return nil, err
	}
	var arr []interface{}
	if err := json.Unmarshal(b, &arr); err != nil {
		return nil, err
	}
	var first M
	for _, it := range arr {
		m := asM(it)
		if m == nil {
			continue
		}
		if first == nil {
			first = m
		}
		if asS(m["version_type"]) == "release" {
			return m, nil
		}
	}
	if first == nil {
		return nil, errors.New("sem versao para " + game)
	}
	return first, nil
}

// collectMods resolve cada mod (e suas dependencias obrigatorias) no Modrinth.
func collectMods(game, modsDir string, slugs []string) []Job {
	var jobs []Job
	seen := map[string]bool{}
	var add func(proj string, depth int) bool
	add = func(proj string, depth int) bool {
		v, err := modrinthLatest(proj, game)
		if err != nil {
			fmt.Printf("  - %s: %v (ignorado)\n", proj, err)
			return false
		}
		pid := asS(v["project_id"])
		if seen[pid] {
			return true
		}
		seen[pid] = true
		var f M
		for _, fo := range asL(v["files"]) {
			fm := asM(fo)
			if f == nil {
				f = fm
			}
			if asB(fm["primary"]) {
				f = fm
			}
		}
		if f == nil {
			return false
		}
		name := filepath.Base(asS(f["filename"]))
		jobs = append(jobs, Job{URL: asS(f["url"]), Dest: filepath.Join(modsDir, name), SHA1: asS(asM(f["hashes"])["sha1"]), Size: asN(f["size"])})
		fmt.Println("  + " + proj)
		if depth < 3 {
			for _, d := range asL(v["dependencies"]) {
				dm := asM(d)
				if asS(dm["dependency_type"]) == "required" && asS(dm["project_id"]) != "" {
					add(asS(dm["project_id"]), depth+1)
				}
			}
		}
		return true
	}
	for _, sl := range slugs {
		for _, alt := range strings.Split(sl, "|") {
			if add(alt, 0) {
				break
			}
		}
	}
	return jobs
}

// cleanOldMods remove mods que o launcher instalou antes e que nao fazem mais parte da lista.
func cleanOldMods(dir string, jobs []Job) {
	keep := map[string]bool{}
	for _, j := range jobs {
		keep[filepath.Base(j.Dest)] = true
	}
	listPath := filepath.Join(dir, ".gerenciados")
	if b, err := os.ReadFile(listPath); err == nil {
		for _, name := range strings.Split(string(b), "\n") {
			name = strings.TrimSpace(name)
			if name != "" && !keep[name] {
				os.Remove(filepath.Join(dir, name))
			}
		}
	}
	var names []string
	for n := range keep {
		names = append(names, n)
	}
	os.WriteFile(listPath, []byte(strings.Join(names, "\n")), 0644)
}

// ---------- lancamento ----------

func launch(base, dados string, v *Ver, nome string, ram int, semSom bool, nivel int) error {
	otim := nivel > 0
	nome = regexp.MustCompile(`[^A-Za-z0-9_]`).ReplaceAllString(nome, "")
	if len(nome) < 3 {
		nome = "Jogador"
	}
	if len(nome) > 16 {
		nome = nome[:16]
	}
	arch := "32"
	if is64() {
		arch = "64"
	}

	libs := filepath.Join(dados, "libs")
	assets := filepath.Join(dados, "assets")
	vdir := filepath.Join(dados, "versoes", v.ID)
	natives := filepath.Join(vdir, "natives")
	jogo := filepath.Join(dados, "jogo", v.ID)
	for _, d := range []string{vdir, natives} {
		os.MkdirAll(d, 0755)
	}

	// 1. JSON da versao
	vjson := filepath.Join(vdir, v.ID+".json")
	if !fileExists(vjson) {
		fmt.Println("Buscando dados da versao...")
		b, err := getBytes(v.URL)
		if err != nil {
			return err
		}
		if err := os.WriteFile(vjson, b, 0644); err != nil {
			return err
		}
	}
	ver, err := readJSON(vjson)
	if err != nil {
		return err
	}

	modernOpts := asM(ver["arguments"]) != nil

	// Modo otimizado: Fabric (ou Legacy Fabric) + mods de desempenho, se a versao for suportada
	if otim {
		applied := false
		for _, ld := range loaderDefs {
			fmt.Printf("Otimizacao nivel %d: preparando %s...\n", nivel, ld.name)
			err := applyFabric(vdir, v.ID, ver, ld)
			if err == nil {
				applied = true
				break
			}
			fmt.Println("  " + ld.name + " indisponivel: " + err.Error())
		}
		if !applied {
			fmt.Println("Nenhum loader suporta esta versao. Abrindo a versao normal.")
			otim = false
		}
	}
	if otim {
		jogo = jogo + "-otimizado"
	}
	os.MkdirAll(jogo, 0755)
	var modJobs []Job
	if otim {
		fmt.Println("Procurando mods de desempenho (Modrinth)...")
		modsDir := filepath.Join(jogo, "mods")
		os.MkdirAll(modsDir, 0755)
		modJobs = collectMods(v.ID, modsDir, modsForLevel(nivel))
		cleanOldMods(modsDir, modJobs)
	}

	// 2. Java
	component, major := "jre-legacy", 8
	if jv := asM(ver["javaVersion"]); jv != nil {
		if c := asS(jv["component"]); c != "" {
			component = c
		}
		if mj := int(asN(jv["majorVersion"])); mj > 0 {
			major = mj
		}
	}
	if major > 8 && ram < 1024 {
		fmt.Printf("AVISO: esta versao pede Java %d e costuma precisar de bem mais que %d MB de RAM.\n", major, ram)
	}
	java, err := ensureJava(base, dados, component, major)
	if err != nil {
		return err
	}

	// 3. Jar, bibliotecas, log config, indice de assets
	var jobs []Job
	var cp []string
	var natList []string
	seen := map[string]bool{}

	cdl := asM(asM(ver["downloads"])["client"])
	if cdl == nil {
		return errors.New("esta versao nao tem client.jar para baixar")
	}
	jar := filepath.Join(vdir, v.ID+".jar")
	jobs = append(jobs, Job{URL: asS(cdl["url"]), Dest: jar, SHA1: asS(cdl["sha1"]), Size: asN(cdl["size"])})

	for _, lo := range asL(ver["libraries"]) {
		lib := asM(lo)
		if lib == nil || !rulesOk(lib["rules"]) {
			continue
		}
		dl := asM(lib["downloads"])
		if art := asM(dl["artifact"]); art != nil {
			pa := filepath.Join(libs, filepath.FromSlash(asS(art["path"])))
			if !seen[pa] {
				seen[pa] = true
				jobs = append(jobs, Job{URL: asS(art["url"]), Dest: pa, SHA1: asS(art["sha1"]), Size: asN(art["size"])})
				cp = append(cp, pa)
			}
		}
		if nat := asM(lib["natives"]); nat != nil && asS(nat["windows"]) != "" {
			cls := strings.Replace(asS(nat["windows"]), "${arch}", arch, -1)
			if cl := asM(asM(dl["classifiers"])[cls]); cl != nil {
				pn := filepath.Join(libs, filepath.FromSlash(asS(cl["path"])))
				jobs = append(jobs, Job{URL: asS(cl["url"]), Dest: pn, SHA1: asS(cl["sha1"]), Size: asN(cl["size"])})
				natList = append(natList, pn)
			}
		}
	}
	cp = append(cp, jar)

	logArg := ""
	if lg := asM(asM(ver["logging"])["client"]); lg != nil {
		if lf := asM(lg["file"]); lf != nil {
			lp := filepath.Join(assets, "log_configs", asS(lf["id"]))
			jobs = append(jobs, Job{URL: asS(lf["url"]), Dest: lp, SHA1: asS(lf["sha1"]), Size: asN(lf["size"])})
			if la := asS(lg["argument"]); la != "" {
				logArg = strings.Replace(la, "${path}", lp, -1)
			}
		}
	}

	ai := asM(ver["assetIndex"])
	aid := asS(ai["id"])
	if aid == "" {
		aid = "legacy"
	}
	idxPath := filepath.Join(assets, "indexes", aid+".json")
	if ai != nil {
		jobs = append(jobs, Job{URL: asS(ai["url"]), Dest: idxPath, SHA1: asS(ai["sha1"]), Size: asN(ai["size"])})
	}

	jobs = append(jobs, modJobs...)
	fmt.Println("Baixando jogo, bibliotecas e mods...")
	if err := downloadAll(jobs); err != nil {
		return err
	}

	// 4. Natives
	for _, zp := range natList {
		extractNatives(zp, natives)
	}

	// 5. Assets
	assetsVirtual := assets
	if fileExists(idxPath) {
		idx, err := readJSON(idxPath)
		if err != nil {
			return err
		}
		virt := asB(idx["virtual"])
		mapRes := asB(idx["map_to_resources"])
		if virt {
			assetsVirtual = filepath.Join(assets, "virtual", aid)
		}
		full := filepath.Join(vdir, ".assets_ok")
		marker := full
		if semSom {
			marker = filepath.Join(vdir, ".assets_nosound")
		}
		if !fileExists(full) && !fileExists(marker) {
			fmt.Println("Baixando assets (so na primeira vez)...")
			var aj []Job
			type cpy struct{ src, dst string }
			var copies []cpy
			for name, ov := range asM(idx["objects"]) {
				if semSom && strings.HasPrefix(name, "minecraft/sounds/") {
					continue
				}
				o := asM(ov)
				h := asS(o["hash"])
				if len(h) < 2 {
					continue
				}
				pre := h[:2]
				op := filepath.Join(assets, "objects", pre, h)
				aj = append(aj, Job{URL: "https://resources.download.minecraft.net/" + pre + "/" + h, Dest: op, SHA1: h, Size: asN(o["size"])})
				rel := filepath.FromSlash(name)
				if virt {
					copies = append(copies, cpy{op, filepath.Join(assetsVirtual, rel)})
				}
				if mapRes {
					copies = append(copies, cpy{op, filepath.Join(jogo, "resources", rel)})
				}
			}
			if err := downloadAll(aj); err != nil {
				return err
			}
			for _, c := range copies {
				if err := copyFile(c.src, c.dst); err != nil {
					return err
				}
			}
			os.WriteFile(marker, []byte("1"), 0644)
		}
	}

	// 6. Opcoes de video minimas (so se ainda nao existirem)
	opt := filepath.Join(jogo, "options.txt")
	if !fileExists(opt) {
		txt := "renderDistance:2\nparticles:2\nfancyGraphics:false\ngraphicsMode:0\nenableVsync:false\nmaxFps:40\nmipmapLevels:0\nentityShadows:false\nviewBobbing:false\n"
		if modernOpts {
			txt = "renderDistance:4\nsimulationDistance:5\nparticles:2\ngraphicsMode:0\nao:false\nenableVsync:false\nmaxFps:60\nmipmapLevels:0\nbiomeBlendRadius:0\nentityShadows:false\nentityDistanceScaling:0.5\nviewBobbing:false\nrenderClouds:\"false\"\n"
		}
		os.WriteFile(opt, []byte(txt), 0644)
	}

	// 7. UUID offline
	sum := md5.Sum([]byte("OfflinePlayer:" + nome))
	b := sum[:]
	b[6] = (b[6] & 0x0f) | 0x30
	b[8] = (b[8] & 0x3f) | 0x80
	uuid := hex.EncodeToString(b)

	// 8. Argumentos
	cpStr := strings.Join(cp, ";")
	vtype := asS(ver["type"])
	if vtype == "" {
		vtype = "release"
	}
	vars := map[string]string{
		"auth_player_name":    nome,
		"version_name":        v.ID,
		"game_directory":      jogo,
		"assets_root":         assets,
		"game_assets":         assetsVirtual,
		"assets_index_name":   aid,
		"auth_uuid":           uuid,
		"auth_access_token":   "0",
		"auth_session":        "0",
		"clientid":            "0",
		"auth_xuid":           "0",
		"user_type":           "legacy",
		"user_properties":     "{}",
		"version_type":        vtype,
		"natives_directory":   natives,
		"launcher_name":       "MiniLauncher",
		"launcher_version":    "1.0",
		"classpath":           cpStr,
		"library_directory":   libs,
		"classpath_separator": ";",
	}
	var pairs []string
	for k, val := range vars {
		pairs = append(pairs, "${"+k+"}", val)
	}
	repl := strings.NewReplacer(pairs...)

	var jvmArgs, gameArgs []string
	if ao := asM(ver["arguments"]); ao != nil {
		jvmArgs = collectArgs(ao["jvm"])
		gameArgs = collectArgs(ao["game"])
	} else {
		gameArgs = strings.Fields(asS(ver["minecraftArguments"]))
		jvmArgs = []string{"-Djava.library.path=${natives_directory}"}
	}
	if contains(gameArgs, "--gameDir") && !contains(gameArgs, "--width") {
		gameArgs = append(gameArgs, "--width", "640", "--height", "480")
	}

	cmdArgs := []string{
		"-XX:+IgnoreUnrecognizedVMOptions",
		fmt.Sprintf("-Xmx%dM", ram),
		fmt.Sprintf("-Xms%dM", minInt(ram, 256)),
		"-XX:+UseSerialGC",
		"-XX:TieredStopAtLevel=1",
		"-XX:ReservedCodeCacheSize=64m",
		"-XX:-UseBiasedLocking",
		"-Djava.net.preferIPv4Stack=true",
		"-Dfile.encoding=UTF-8",
		"-Dlog4j2.formatMsgNoLookups=true",
	}
	if logArg != "" {
		cmdArgs = append(cmdArgs, logArg)
	}
	hasCp := false
	for _, a := range jvmArgs {
		if a == "-cp" || a == "-classpath" {
			hasCp = true
		}
		cmdArgs = append(cmdArgs, repl.Replace(a))
	}
	if !hasCp {
		cmdArgs = append(cmdArgs, "-cp", cpStr)
	}
	cmdArgs = append(cmdArgs, asS(ver["mainClass"]))
	for _, g := range gameArgs {
		cmdArgs = append(cmdArgs, repl.Replace(g))
	}

	logPath := filepath.Join(dados, "log.txt")
	lf, err := os.Create(logPath)
	if err != nil {
		return err
	}
	defer lf.Close()
	cmd := exec.Command(java, cmdArgs...)
	cmd.Dir = jogo
	cmd.Stdout = lf
	cmd.Stderr = lf
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00008000} // prioridade acima do normal
	start := time.Now()
	fmt.Printf("Iniciando %s (Java %d, %d MB)...\n", v.ID, major, ram)
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := cmd.Wait(); err != nil {
		tail(logPath, 15)
		if otim && time.Since(start) < 120*time.Second {
			return fmt.Errorf("%w (%v); log em %s", errCrash, err, logPath)
		}
		return fmt.Errorf("o jogo fechou com erro: %v (log completo em %s)", err, logPath)
	}
	return nil
}

// ---------- menu ----------

func loadVersions(dados string) ([]Ver, error) {
	cache := filepath.Join(dados, "version_manifest.json")
	m, err := getJSON(manifestURL)
	if err == nil {
		if b, e := json.Marshal(m); e == nil {
			os.WriteFile(cache, b, 0644)
		}
	} else {
		m, err = readJSON(cache)
		if err != nil {
			return nil, errors.New("sem internet e sem lista de versoes salva")
		}
	}
	latestRelease = asS(asM(m["latest"])["release"])
	var out []Ver
	for _, v := range asL(m["versions"]) {
		d := asM(v)
		out = append(out, Ver{ID: asS(d["id"]), Type: asS(d["type"]), URL: asS(d["url"])})
	}
	return out, nil
}

func findVer(vers []Ver, id string) *Ver {
	for i := range vers {
		if vers[i].ID == id {
			return &vers[i]
		}
	}
	return nil
}

func printList(vers []Ver, ask func(string, string) string) {
	c := ask("Mostrar: 1=releases 2=snapshots 3=beta 4=alpha", "1")
	want := map[string]string{"1": "release", "2": "snapshot", "3": "old_beta", "4": "old_alpha"}[c]
	line := ""
	for _, v := range vers {
		if v.Type != want {
			continue
		}
		if len(line)+len(v.ID)+2 > 56 {
			fmt.Println(line)
			line = ""
		}
		line += v.ID + "  "
	}
	if line != "" {
		fmt.Println(line)
	}
}

func run() int {
	exe, _ := os.Executable()
	base := filepath.Dir(exe)
	dados := filepath.Join(base, "dados")
	os.MkdirAll(dados, 0755)
	in := bufio.NewReader(os.Stdin)
	ask := func(q, def string) string {
		fmt.Printf("%s [%s]: ", q, def)
		s, _ := in.ReadString('\n')
		s = strings.TrimSpace(s)
		if s == "" {
			return def
		}
		return s
	}

	fmt.Println("=== MiniLauncher - Minecraft Java (todas as versoes) ===")
	vers, err := loadVersions(dados)
	if err != nil {
		fmt.Println("ERRO:", err)
		return 1
	}
	fmt.Printf("%d versoes disponiveis.\n", len(vers))

	// uso rapido: MiniLauncher.exe 1.8.9 Nick 400
	args := os.Args[1:]
	id, nome, ramStr := "", "", ""
	if len(args) > 0 {
		id = args[0]
	}
	if len(args) > 1 {
		nome = args[1]
	}
	if len(args) > 2 {
		ramStr = args[2]
	}

	for {
		if id == "" {
			id = ask("Versao (ex 1.8.9), 'ultima' ou 'lista'", "1.8.9")
		}
		if strings.EqualFold(id, "lista") {
			printList(vers, ask)
			id = ""
			continue
		}
		if strings.EqualFold(id, "ultima") && latestRelease != "" {
			id = latestRelease
			fmt.Println("Ultima versao:", id)
		}
		if findVer(vers, id) != nil {
			break
		}
		fmt.Println("Versao nao encontrada:", id)
		id = ""
	}
	if nome == "" {
		nome = ask("Nick", "Jogador")
	}
	if ramStr == "" {
		ramStr = ask("RAM em MB", "1024")
	}
	ram, err := strconv.Atoi(ramStr)
	if err != nil || ram < 128 {
		ram = 1024
	}
	semSom := false
	if len(args) == 0 {
		semSom = strings.HasPrefix(strings.ToLower(ask("Pular sons para baixar menos? (s/n)", "n")), "s")
	}

	nivel := 0
	if len(args) == 0 {
		n := ask("Otimizacao: 0=nenhuma 1=segura 2=forte 3=maxima", "3")
		if x, e := strconv.Atoi(n); e == nil && x >= 0 && x <= 3 {
			nivel = x
		} else {
			nivel = 3
		}
	} else if contains(args, "otimizado") {
		nivel = 3
	}

	for {
		err := launch(base, dados, findVer(vers, id), nome, ram, semSom, nivel)
		if err == nil {
			return 0
		}
		if errors.Is(err, errCrash) && nivel > 1 {
			nivel--
			fmt.Printf("\nO jogo fechou com erro. Tentando de novo com menos mods (nivel %d)...\n\n", nivel)
			continue
		}
		fmt.Println("\nERRO:", err)
		return 1
	}
}

func main() {
	code := run()
	if code != 0 {
		fmt.Println("\nPressione Enter para fechar...")
		bufio.NewReader(os.Stdin).ReadString('\n')
	}
	os.Exit(code)
}

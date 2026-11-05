// MiniLauncher - launcher leve de Minecraft Java (todas as versoes da Mojang).
// Gera um .exe unico, sem dependencias (sem .NET, sem Java previo).
package main

import (
	"archive/zip"
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
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

var client = &http.Client{Transport: newTransport(false), Timeout: 15 * time.Minute}
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

func httpGet(url string) (*http.Response, error) {
	resp, err := client.Get(url)
	if err != nil && strings.Contains(err.Error(), "x509") {
		tlsMu.Lock()
		if !insecure {
			insecure = true
			client.Transport = newTransport(true)
			fmt.Println("AVISO: certificados do sistema nao reconhecidos; seguindo sem verificar TLS (arquivos conferidos por SHA1).")
		}
		tlsMu.Unlock()
		resp, err = client.Get(url)
	}
	return resp, err
}

func getBytes(url string) ([]byte, error) {
	resp, err := httpGet(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d em %s", resp.StatusCode, url)
	}
	return io.ReadAll(resp.Body)
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
	resp, err := httpGet(url)
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
	_, err = io.Copy(io.MultiWriter(f, h), resp.Body)
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
				if n%20 == 0 || int(n) == total {
					fmt.Printf("\r  %d/%d arquivos", n, total)
				}
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

// ---------- lancamento ----------

func launch(base, dados string, v *Ver, nome string, ram int, semSom bool) error {
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
	for _, d := range []string{vdir, natives, jogo} {
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

	fmt.Println("Baixando jogo e bibliotecas...")
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
		os.WriteFile(opt, []byte("renderDistance:2\nparticles:2\nfancyGraphics:false\ngraphicsMode:0\nenableVsync:false\nmaxFps:40\nmipmapLevels:0\nentityShadows:false\nviewBobbing:false\n"), 0644)
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
		fmt.Sprintf("-Xmx%dM", ram),
		fmt.Sprintf("-Xms%dM", minInt(ram, 256)),
		"-XX:+UseSerialGC",
		"-XX:TieredStopAtLevel=1",
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
	fmt.Printf("Iniciando %s (Java %d, %d MB)...\n", v.ID, major, ram)
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := cmd.Wait(); err != nil {
		tail(logPath, 15)
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
			id = ask("Versao (ex 1.8.9) ou 'lista'", "1.8.9")
		}
		if strings.EqualFold(id, "lista") {
			printList(vers, ask)
			id = ""
			continue
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

	if err := launch(base, dados, findVer(vers, id), nome, ram, semSom); err != nil {
		fmt.Println("\nERRO:", err)
		return 1
	}
	return 0
}

func main() {
	code := run()
	if code != 0 {
		fmt.Println("\nPressione Enter para fechar...")
		bufio.NewReader(os.Stdin).ReadString('\n')
	}
	os.Exit(code)
}

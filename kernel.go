package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	singBoxReleasesAPI = "https://api.github.com/repos/SagerNet/sing-box/releases"

	// Each release lists ~40 assets with long metadata: 15 releases is ~5 MB.
	maxReleaseListBytes   = 12 << 20
	releaseListPageSize   = 15
	maxKernelArchiveBytes = 256 << 20
	maxKernelBinaryBytes  = 512 << 20
	releaseAPITimeout     = 30 * time.Second
	kernelDownloadTimeout = 15 * time.Minute
	kernelVersionTimeout  = 10 * time.Second

	kernelSourceManaged = "managed"
	kernelSourceSystem  = "system"
	kernelSourceNone    = "none"
)

var (
	stableTagPattern  = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)
	releaseTagPattern = regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z.]+)?$`)
)

// Release assets are named after Go's architecture names, except 32-bit arm.
var releaseArch = map[string]string{
	"amd64": "amd64",
	"arm64": "arm64",
	"arm":   "armv7", // sbpanel's own arm build targets GOARM=7
	"386":   "386",
}

// Kernel tracks which sing-box binary is used. A binary installed from the panel
// ("managed", kept in the data directory) takes precedence over the system one, so
// the panel never has to write to /usr/bin.
type Kernel struct {
	system string
	dir    string

	mu       sync.Mutex
	versions map[string]cachedVersion
	releases *KernelReleases
	job      KernelJob
}

type cachedVersion struct {
	modTime time.Time
	size    int64
	version string
}

type KernelRelease struct {
	Tag         string    `json:"tag"`
	Version     string    `json:"version"`
	PublishedAt time.Time `json:"publishedAt"`
}

type KernelReleases struct {
	Stable     *KernelRelease `json:"stable,omitempty"`
	Prerelease *KernelRelease `json:"prerelease,omitempty"`
	CheckedAt  time.Time      `json:"checkedAt"`
	Via        string         `json:"via"`
}

type KernelJob struct {
	Running    bool       `json:"running"`
	Tag        string     `json:"tag,omitempty"`
	Stage      string     `json:"stage,omitempty"`
	Done       int64      `json:"done,omitempty"`
	Total      int64      `json:"total,omitempty"`
	Error      string     `json:"error,omitempty"`
	Message    string     `json:"message,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
}

func NewKernel(system, dir string) *Kernel {
	return &Kernel{system: system, dir: dir, versions: map[string]cachedVersion{}}
}

func (k *Kernel) managedPath() string   { return filepath.Join(k.dir, "sing-box") }
func (k *Kernel) previousPath() string  { return filepath.Join(k.dir, "sing-box.previous") }
func (k *Kernel) candidatePath() string { return filepath.Join(k.dir, "sing-box.candidate") }
func (k *Kernel) archivePath() string   { return filepath.Join(k.dir, "download.tar.gz") }

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// Active returns the binary sing-box is started with.
func (k *Kernel) Active() string {
	if fileExists(k.managedPath()) {
		return k.managedPath()
	}
	return k.system
}

func (k *Kernel) Source() string {
	switch {
	case fileExists(k.managedPath()):
		return kernelSourceManaged
	case k.system != "":
		return kernelSourceSystem
	}
	return kernelSourceNone
}

// Version returns the version of the binary at path, cached until the file changes.
func (k *Kernel) Version(path string) string {
	if path == "" {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	k.mu.Lock()
	cached, ok := k.versions[path]
	k.mu.Unlock()
	if ok && cached.modTime.Equal(info.ModTime()) && cached.size == info.Size() {
		return cached.version
	}
	ctx, cancel := context.WithTimeout(context.Background(), kernelVersionTimeout)
	defer cancel()
	version, err := binaryVersion(ctx, path)
	if err != nil {
		version = ""
	}
	k.mu.Lock()
	k.versions[path] = cachedVersion{modTime: info.ModTime(), size: info.Size(), version: version}
	k.mu.Unlock()
	return version
}

// binaryVersion runs `<path> version` and checks that the file really is sing-box.
func binaryVersion(ctx context.Context, path string) (string, error) {
	output, err := exec.CommandContext(ctx, path, "version").Output()
	if err != nil {
		return "", fmt.Errorf("无法运行 %s: %w", filepath.Base(path), err)
	}
	firstLine := strings.TrimSpace(strings.SplitN(string(output), "\n", 2)[0])
	const prefix = "sing-box version "
	if !strings.HasPrefix(firstLine, prefix) {
		return "", errors.New("这个文件不是 sing-box")
	}
	return strings.TrimPrefix(firstLine, prefix), nil
}

func releaseAssetName(tag string) (string, error) {
	arch, ok := releaseArch[runtime.GOARCH]
	if !ok || runtime.GOOS != "linux" {
		return "", fmt.Errorf("暂不支持在 %s/%s 上自动升级", runtime.GOOS, runtime.GOARCH)
	}
	// The musl build is a single static binary with every feature (naive included).
	return fmt.Sprintf("sing-box-%s-linux-%s-musl.tar.gz", strings.TrimPrefix(tag, "v"), arch), nil
}

// ---- view ----

type KernelBinaryView struct {
	Path    string `json:"path"`
	Version string `json:"version"`
}

type KernelView struct {
	Active    KernelBinaryView  `json:"active"`
	Source    string            `json:"source"`
	System    *KernelBinaryView `json:"system,omitempty"`
	Managed   *KernelBinaryView `json:"managed,omitempty"`
	Previous  *KernelBinaryView `json:"previous,omitempty"`
	Releases  *KernelReleases   `json:"releases,omitempty"`
	Job       KernelJob         `json:"job"`
	Supported bool              `json:"supported"`
}

func (k *Kernel) View() KernelView {
	binaryView := func(path string) *KernelBinaryView {
		if path == "" || !fileExists(path) {
			return nil
		}
		return &KernelBinaryView{Path: path, Version: k.Version(path)}
	}
	active := k.Active()
	view := KernelView{
		Active:   KernelBinaryView{Path: active, Version: k.Version(active)},
		Source:   k.Source(),
		System:   binaryView(k.system),
		Managed:  binaryView(k.managedPath()),
		Previous: binaryView(k.previousPath()),
	}
	_, err := releaseAssetName("v0.0.0")
	view.Supported = err == nil
	k.mu.Lock()
	if k.releases != nil {
		releases := *k.releases
		view.Releases = &releases
	}
	view.Job = k.job
	k.mu.Unlock()
	return view
}

func (k *Kernel) updateJob(change func(*KernelJob)) {
	k.mu.Lock()
	defer k.mu.Unlock()
	change(&k.job)
}

// ---- GitHub releases ----

type githubAsset struct {
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Digest             string `json:"digest"`
}

type githubRelease struct {
	TagName     string        `json:"tag_name"`
	Prerelease  bool          `json:"prerelease"`
	Draft       bool          `json:"draft"`
	PublishedAt time.Time     `json:"published_at"`
	Assets      []githubAsset `json:"assets"`
}

func githubHeader() http.Header {
	return http.Header{
		"Accept":               {"application/vnd.github+json"},
		"X-Github-Api-Version": {"2022-11-28"},
		"User-Agent":           {"sbpanel/" + version},
	}
}

func (a *App) fetchGitHubJSON(ctx context.Context, rawURL string, target any) (string, error) {
	return a.fetchWithFallback(ctx, rawURL, githubHeader(), releaseAPITimeout, func(response *http.Response) error {
		data, err := readLimited(response.Body, maxReleaseListBytes)
		if err != nil {
			return err
		}
		return json.Unmarshal(data, target)
	})
}

// CheckKernelReleases looks up the newest stable and pre-release sing-box versions.
// It only runs when asked: nothing in sbpanel polls GitHub on its own.
func (a *App) CheckKernelReleases(ctx context.Context) error {
	var releases []githubRelease
	// Pre-releases come out every few days, so the newest stable is always within one page.
	via, err := a.fetchGitHubJSON(ctx, singBoxReleasesAPI+"?per_page="+strconv.Itoa(releaseListPageSize), &releases)
	if err != nil {
		return fmt.Errorf("获取 sing-box 版本列表失败: %w", err)
	}
	result := &KernelReleases{CheckedAt: time.Now(), Via: via}
	for _, release := range releases {
		if release.Draft {
			continue
		}
		entry := &KernelRelease{Tag: release.TagName, Version: strings.TrimPrefix(release.TagName, "v"), PublishedAt: release.PublishedAt}
		if !release.Prerelease && stableTagPattern.MatchString(release.TagName) && result.Stable == nil {
			result.Stable = entry
		}
		if release.Prerelease && releaseTagPattern.MatchString(release.TagName) && result.Prerelease == nil {
			result.Prerelease = entry
		}
	}
	if result.Stable != nil && result.Prerelease != nil && result.Prerelease.PublishedAt.Before(result.Stable.PublishedAt) {
		result.Prerelease = nil // the newest stable already supersedes it
	}
	a.kernel.mu.Lock()
	a.kernel.releases = result
	a.kernel.mu.Unlock()
	return nil
}

// ---- install / rollback ----

// StartKernelInstall downloads and installs a sing-box release in the background.
func (a *App) StartKernelInstall(tag string) error {
	if !releaseTagPattern.MatchString(tag) {
		return errors.New("版本号格式不对")
	}
	if _, err := releaseAssetName(tag); err != nil {
		return err
	}
	k := a.kernel
	k.mu.Lock()
	if k.job.Running {
		k.mu.Unlock()
		return errors.New("已经有一个升级任务在进行")
	}
	k.job = KernelJob{Running: true, Tag: tag, Stage: "准备中"}
	k.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), kernelDownloadTimeout)
		defer cancel()
		message, err := a.installKernel(ctx, tag)
		now := time.Now()
		k.updateJob(func(job *KernelJob) {
			job.Running = false
			job.Stage = ""
			job.FinishedAt = &now
			job.Message = message
			if err != nil {
				job.Error = err.Error()
			}
		})
		if err != nil {
			log.Printf("安装 sing-box %s 失败: %v", tag, err)
		} else {
			log.Printf("%s", message)
		}
	}()
	return nil
}

func (a *App) installKernel(ctx context.Context, tag string) (string, error) {
	k := a.kernel
	if err := os.MkdirAll(k.dir, 0o700); err != nil {
		return "", err
	}
	assetName, err := releaseAssetName(tag)
	if err != nil {
		return "", err
	}

	k.updateJob(func(job *KernelJob) { job.Stage = "获取版本信息" })
	var release githubRelease
	if _, err := a.fetchGitHubJSON(ctx, singBoxReleasesAPI+"/tags/"+tag, &release); err != nil {
		return "", fmt.Errorf("获取版本 %s 的信息失败: %w", tag, err)
	}
	var asset *githubAsset
	for i := range release.Assets {
		if release.Assets[i].Name == assetName {
			asset = &release.Assets[i]
		}
	}
	if asset == nil {
		return "", fmt.Errorf("版本 %s 没有 %s 这个安装包", tag, assetName)
	}
	wantDigest, hasDigest := strings.CutPrefix(asset.Digest, "sha256:")
	if !hasDigest {
		return "", errors.New("GitHub 没有提供这个安装包的 SHA-256，无法校验，已取消")
	}

	k.updateJob(func(job *KernelJob) { job.Stage = "下载中"; job.Total = asset.Size })
	archive := k.archivePath()
	defer os.Remove(archive)
	via, err := a.fetchWithFallback(ctx, asset.BrowserDownloadURL, http.Header{"User-Agent": {"sbpanel/" + version}},
		kernelDownloadTimeout, func(response *http.Response) error {
			return downloadVerified(response.Body, archive, wantDigest, func(done int64) {
				k.updateJob(func(job *KernelJob) { job.Done = done })
			})
		})
	if err != nil {
		return "", fmt.Errorf("下载失败: %w", err)
	}

	k.updateJob(func(job *KernelJob) { job.Stage = "解压与校验" })
	candidate := k.candidatePath()
	defer os.Remove(candidate)
	if err := extractSingBox(archive, candidate); err != nil {
		return "", fmt.Errorf("解压失败: %w", err)
	}
	gotVersion, err := binaryVersion(ctx, candidate)
	if err != nil {
		return "", err
	}
	if gotVersion != strings.TrimPrefix(tag, "v") {
		return "", fmt.Errorf("安装包里的版本是 %s，和 %s 不一致", gotVersion, tag)
	}

	k.updateJob(func(job *KernelJob) { job.Stage = "切换内核" })
	managed, previous := k.managedPath(), k.previousPath()
	err = a.switchKernel(ctx, candidate, func() (func() error, error) {
		hadManaged := fileExists(managed)
		if hadManaged {
			if err := os.Rename(managed, previous); err != nil {
				return nil, err
			}
		}
		if err := os.Rename(candidate, managed); err != nil {
			if hadManaged {
				_ = os.Rename(previous, managed)
			}
			return nil, err
		}
		return func() error {
			if err := os.Remove(managed); err != nil {
				return err
			}
			if hadManaged {
				return os.Rename(previous, managed)
			}
			return nil
		}, nil
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("sing-box 已升级到 %s（通过%s下载，SHA-256 校验通过）", gotVersion, via), nil
}

// downloadVerified streams body into path while hashing it, and rejects a mismatch.
func downloadVerified(body io.Reader, path, wantSHA256 string, progress func(int64)) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	hasher := sha256.New()
	counter := &progressWriter{report: progress}
	_, copyErr := io.Copy(io.MultiWriter(file, hasher, counter), io.LimitReader(body, maxKernelArchiveBytes+1))
	closeErr := file.Close()
	if copyErr != nil {
		return stripURL(copyErr)
	}
	if closeErr != nil {
		return closeErr
	}
	if counter.total > maxKernelArchiveBytes {
		return errors.New("安装包太大")
	}
	if got := hex.EncodeToString(hasher.Sum(nil)); !strings.EqualFold(got, wantSHA256) {
		return fmt.Errorf("SHA-256 不一致（期望 %s，实际 %s）", wantSHA256[:12], got[:12])
	}
	return nil
}

type progressWriter struct {
	total    int64
	reported time.Time
	report   func(int64)
}

func (w *progressWriter) Write(data []byte) (int, error) {
	w.total += int64(len(data))
	if time.Since(w.reported) > 300*time.Millisecond {
		w.reported = time.Now()
		w.report(w.total)
	}
	return len(data), nil
}

// extractSingBox copies the `sing-box` executable out of a release tarball.
func extractSingBox(archive, destination string) error {
	return extractFromTarball(archive, "sing-box", destination, 0o700)
}

// extractFromTarball copies the regular file called name (at any depth) out of a
// .tar.gz archive.
func extractFromTarball(archive, name, destination string, mode os.FileMode) error {
	file, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	reader := tar.NewReader(gzipReader)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return fmt.Errorf("安装包里没有 %s", name)
		}
		if err != nil {
			return err
		}
		if header.Typeflag != tar.TypeReg || filepath.Base(header.Name) != name {
			continue
		}
		if header.Size > maxKernelBinaryBytes {
			return fmt.Errorf("%s 文件太大", name)
		}
		output, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(output, io.LimitReader(reader, maxKernelBinaryBytes))
		closeErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	}
}

// RollbackKernel switches to the previously used panel-installed kernel.
func (a *App) RollbackKernel(ctx context.Context) error {
	k := a.kernel
	managed, previous := k.managedPath(), k.previousPath()
	if !fileExists(previous) {
		return errors.New("没有可以回退的旧版本")
	}
	swapAside := filepath.Join(k.dir, "sing-box.swap")
	return a.switchKernel(ctx, previous, func() (func() error, error) {
		if !fileExists(managed) {
			if err := os.Rename(previous, managed); err != nil {
				return nil, err
			}
			return func() error { return os.Rename(managed, previous) }, nil
		}
		swap := func() error {
			if err := os.Rename(managed, swapAside); err != nil {
				return err
			}
			if err := os.Rename(previous, managed); err != nil {
				_ = os.Rename(swapAside, managed)
				return err
			}
			return os.Rename(swapAside, previous)
		}
		if err := swap(); err != nil {
			return nil, err
		}
		return swap, nil // swapping again restores the original order
	})
}

// UseSystemKernel stops using the panel-installed kernel; it is kept for rollback.
func (a *App) UseSystemKernel(ctx context.Context) error {
	k := a.kernel
	managed, previous := k.managedPath(), k.previousPath()
	if !fileExists(managed) {
		return errors.New("现在用的已经是系统自带的 sing-box")
	}
	if k.system == "" || !fileExists(k.system) {
		return errors.New("系统里没有安装 sing-box")
	}
	return a.switchKernel(ctx, k.system, func() (func() error, error) {
		if err := os.Rename(managed, previous); err != nil {
			return nil, err
		}
		return func() error { return os.Rename(previous, managed) }, nil
	})
}

// switchKernel replaces the active binary as one transaction: the current config must
// pass `sing-box check` with the new binary first, and if sing-box then fails to start
// the old binary is put back and started again.
func (a *App) switchKernel(ctx context.Context, next string, swap func() (undo func() error, err error)) error {
	a.applyMu.Lock()
	defer a.applyMu.Unlock()

	state := a.store.Snapshot()
	if hasSelectableNodes(state) {
		checker := &singBoxChecker{binary: fixedBinary(next), tempDir: a.tempDir}
		if _, _, err := buildChecked(ctx, checker, a.buildInput(state)); err != nil && !errors.Is(err, errNoNodes) {
			return fmt.Errorf("新内核不接受当前配置，没有切换: %w", err)
		}
	}

	if err := a.core.Stop(); err != nil {
		return err
	}
	undo, err := swap()
	if err != nil {
		_ = a.apply(ctx, true)
		return fmt.Errorf("替换内核文件失败: %w", err)
	}
	err = a.apply(ctx, true)
	if err == nil {
		return nil
	}

	_ = a.core.Stop()
	if undoErr := undo(); undoErr != nil {
		return fmt.Errorf("新内核启动失败: %v；恢复旧内核也失败: %w", err, undoErr)
	}
	if restartErr := a.apply(ctx, true); restartErr != nil {
		return fmt.Errorf("新内核启动失败: %v；旧内核也没能启动: %w", err, restartErr)
	}
	return fmt.Errorf("新内核启动失败，已换回原来的内核: %w", err)
}

package main

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/client"
)

// Volumes whose names say they hold a database, broker log or search index.
// These are never suggested for removal even when docker reports them unused:
// an unreferenced postgres volume on a shared dev box is usually a detached
// data directory someone still wants, not garbage.
var dataVolumeRe = regexp.MustCompile(`(?i)(pgdata|postgres|psql|mysql|maria|mongo|redis|kafka|zookeeper|elastic|es[-_]?data|clickhouse|minio|rabbit|influx|_data$|-data$)`)

type volRow struct {
	Name      string
	Size      int64
	RefCount  int64
	Driver    string
	CreatedAt string
	Protected bool
}

func handleStatsDisk(minVolumeBytes int) (string, error) {
	ctx, cancel := ctxWithTimeout()
	defer cancel()

	du, err := dockerCli.DiskUsage(ctx, client.DiskUsageOptions{
		Containers: true, Images: true, Volumes: true, BuildCache: true, Verbose: true,
	})
	if err != nil {
		return "", fmt.Errorf("docker disk usage: %w", err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Disk status - %s cluster\n\n", appCfg.SSHHost)

	// ---- host filesystems ----
	fsRows, fsErr := hostFilesystems()
	level, headline := "OK", ""
	var worst hostFS
	if fsErr == nil && len(fsRows) > 0 {
		b.WriteString("HOST FILESYSTEMS\n")
		fmt.Fprintf(&b, "  %-34s %10s %10s %10s %6s  %s\n", "MOUNT", "SIZE", "USED", "AVAIL", "USE%", "STATUS")
		for _, f := range fsRows {
			st := "ok"
			switch {
			case f.UsePct >= appCfg.DiskCritPct:
				st = "CRITICAL"
			case f.UsePct >= appCfg.DiskWarnPct:
				st = "WARNING"
			}
			if f.UsePct > worst.UsePct {
				worst = f
			}
			fmt.Fprintf(&b, "  %-34s %10s %10s %10s %5d%%  %s\n",
				f.Mount, humanBytes(f.Size), humanBytes(f.Used), humanBytes(f.Avail), f.UsePct, st)
		}
		switch {
		case worst.UsePct >= appCfg.DiskCritPct:
			level = "CRITICAL"
			headline = fmt.Sprintf("%s is %d%% full with only %s free (critical threshold %d%%)",
				worst.Mount, worst.UsePct, humanBytes(worst.Avail), appCfg.DiskCritPct)
		case worst.UsePct >= appCfg.DiskWarnPct:
			level = "WARNING"
			headline = fmt.Sprintf("%s is %d%% full with %s free (warning threshold %d%%)",
				worst.Mount, worst.UsePct, humanBytes(worst.Avail), appCfg.DiskWarnPct)
		default:
			headline = fmt.Sprintf("worst mount %s at %d%%, %s free", worst.Mount, worst.UsePct, humanBytes(worst.Avail))
		}
	} else {
		b.WriteString("HOST FILESYSTEMS\n  unavailable: " + errText(fsErr) + "\n")
		headline = "host filesystem usage could not be read; docker figures below are still valid"
		level = "UNKNOWN"
	}

	// ---- docker breakdown ----
	fmt.Fprintf(&b, "\nDOCKER USAGE\n")
	fmt.Fprintf(&b, "  %-14s %6s %7s %12s %14s\n", "TYPE", "TOTAL", "ACTIVE", "SIZE", "RECLAIMABLE")
	fmt.Fprintf(&b, "  %-14s %6d %7d %12s %14s\n", "Images", du.Images.TotalCount, du.Images.ActiveCount, humanBytes(du.Images.TotalSize), humanBytes(du.Images.Reclaimable))
	fmt.Fprintf(&b, "  %-14s %6d %7d %12s %14s\n", "Containers", du.Containers.TotalCount, du.Containers.ActiveCount, humanBytes(du.Containers.TotalSize), humanBytes(du.Containers.Reclaimable))
	fmt.Fprintf(&b, "  %-14s %6d %7d %12s %14s\n", "Volumes", du.Volumes.TotalCount, du.Volumes.ActiveCount, humanBytes(du.Volumes.TotalSize), humanBytes(du.Volumes.Reclaimable))
	fmt.Fprintf(&b, "  %-14s %6d %7d %12s %14s\n", "Build cache", du.BuildCache.TotalCount, du.BuildCache.ActiveCount, humanBytes(du.BuildCache.TotalSize), humanBytes(du.BuildCache.Reclaimable))
	dockerTotal := du.Images.TotalSize + du.Containers.TotalSize + du.Volumes.TotalSize + du.BuildCache.TotalSize
	fmt.Fprintf(&b, "  %-14s %6s %7s %12s\n", "TOTAL", "", "", humanBytes(dockerTotal))

	// ---- volumes ----
	var vols []volRow
	for _, v := range du.Volumes.Items {
		r := volRow{Name: v.Name, Driver: v.Driver, CreatedAt: v.CreatedAt}
		if v.UsageData != nil {
			r.Size = v.UsageData.Size
			r.RefCount = v.UsageData.RefCount
		} else {
			r.RefCount = -1
		}
		r.Protected = dataVolumeRe.MatchString(v.Name)
		vols = append(vols, r)
	}
	sort.Slice(vols, func(i, j int) bool { return vols[i].Size > vols[j].Size })

	fmt.Fprintf(&b, "\nVOLUMES (%d)\n", len(vols))
	fmt.Fprintf(&b, "  %-56s %10s %5s %-10s %s\n", "NAME", "SIZE", "LINKS", "CLASS", "NOTE")
	var orphanData, orphanSafe []volRow
	shown := 0
	for _, v := range vols {
		if int64(minVolumeBytes) > 0 && v.Size < int64(minVolumeBytes) {
			continue
		}
		shown++
		class, note := "in-use", ""
		if v.RefCount == 0 {
			if v.Protected {
				class, note = "ORPHAN*", "unreferenced but data-bearing - PROTECTED, verify before deleting"
				orphanData = append(orphanData, v)
			} else {
				class, note = "ORPHAN", "unreferenced - reclaimable"
				orphanSafe = append(orphanSafe, v)
			}
		} else if v.Protected {
			class = "data"
		} else if isAnonymous(v.Name) {
			class, note = "anon", "anonymous volume held by a container"
		}
		fmt.Fprintf(&b, "  %-56s %10s %5s %-10s %s\n",
			truncStr(v.Name, 56), humanBytes(v.Size), refStr(v.RefCount), class, note)
	}
	if shown < len(vols) {
		fmt.Fprintf(&b, "  (%d volume(s) below the %s size filter not shown)\n", len(vols)-shown, humanBytes(int64(minVolumeBytes)))
	}

	// ---- images worth noting ----
	var dangling, backups []string
	var danglingBytes, backupBytes int64
	for _, img := range du.Images.Items {
		tags := img.RepoTags
		if len(tags) == 0 || (len(tags) == 1 && tags[0] == "<none>:<none>") {
			dangling = append(dangling, shortID(strings.TrimPrefix(img.ID, "sha256:")))
			danglingBytes += img.Size
			continue
		}
		for _, t := range tags {
			if strings.Contains(t, ":backup-") {
				backups = append(backups, fmt.Sprintf("%s (%s)", t, humanBytes(img.Size)))
				backupBytes += img.Size
			}
		}
	}

	// ---- build cache ----
	var cacheInactive int64
	var cacheInactiveCount int
	var oldestInactive time.Time
	for _, c := range du.BuildCache.Items {
		if !c.InUse {
			cacheInactive += c.Size
			cacheInactiveCount++
			if oldestInactive.IsZero() || c.CreatedAt.Before(oldestInactive) {
				oldestInactive = c.CreatedAt
			}
		}
	}

	// ---- verdict and suggestions ----
	fmt.Fprintf(&b, "\nSTATUS: %s - %s\n", level, headline)

	var safeTotal int64
	var sugg []string
	if cacheInactive > 0 {
		age := ""
		if !oldestInactive.IsZero() {
			age = fmt.Sprintf(", oldest %s old", compactDur(time.Since(oldestInactive)))
		}
		sugg = append(sugg, fmt.Sprintf("build cache: %d of %d records are not in use, %s%s\n      docker builder prune --filter until=72h",
			cacheInactiveCount, du.BuildCache.TotalCount, humanBytes(cacheInactive), age))
		safeTotal += cacheInactive
	}
	if len(dangling) > 0 {
		sugg = append(sugg, fmt.Sprintf("dangling images: %d untagged image(s), %s\n      docker image prune",
			len(dangling), humanBytes(danglingBytes)))
		safeTotal += danglingBytes
	}
	if len(orphanSafe) > 0 {
		var names []string
		var sz int64
		for _, v := range orphanSafe {
			names = append(names, fmt.Sprintf("%s (%s)", v.Name, humanBytes(v.Size)))
			sz += v.Size
		}
		sugg = append(sugg, fmt.Sprintf("orphan volumes, no data-bearing name, %s total:\n      %s\n      docker volume rm %s",
			humanBytes(sz), strings.Join(names, "\n      "), strings.Join(volNames(orphanSafe), " ")))
		safeTotal += sz
	}
	if len(backups) > 2 {
		sugg = append(sugg, fmt.Sprintf("rollback images: %d backup tag(s), %s total\n      %s\n      Keep backup-1; backup-3 is the oldest rung of the rotation in build-backend-service.sh and is the first safe one to drop.",
			len(backups), humanBytes(backupBytes), strings.Join(backups, "\n      ")))
	}

	if len(sugg) > 0 {
		b.WriteString("\nRECLAIM SUGGESTIONS (ordered by safety)\n")
		for i, s := range sugg {
			fmt.Fprintf(&b, "  %d. %s\n", i+1, s)
		}
		fmt.Fprintf(&b, "\n  Estimated safe reclaim: %s", humanBytes(safeTotal))
		if fsErr == nil && worst.Size > 0 {
			after := worst.Avail + safeTotal
			fmt.Fprintf(&b, " -> %s free on %s (%d%% used)",
				humanBytes(after), worst.Mount, pctUsed(worst.Size, worst.Size-after))
		}
		b.WriteString("\n")
	} else {
		b.WriteString("\nNothing is safely reclaimable: no dangling images, no unused build cache, no orphan volumes.\n")
	}

	if len(orphanData) > 0 {
		b.WriteString("\nDO NOT DELETE WITHOUT CHECKING\n")
		for _, v := range orphanData {
			fmt.Fprintf(&b, "  %s (%s, LINKS=0, created %s)\n", v.Name, humanBytes(v.Size), v.CreatedAt)
		}
		b.WriteString("  These carry database/broker naming and are unreferenced only because no container currently mounts them.\n")
		b.WriteString("  Confirm with the owner before removing; a detached postgres or kafka volume is not garbage.\n")
	}

	b.WriteString("\nThis server is read-only: it never prunes, deletes or restarts anything. Run the suggested commands yourself after reviewing them.\n")
	return b.String(), nil
}

func volNames(v []volRow) []string {
	out := make([]string, 0, len(v))
	for _, x := range v {
		out = append(out, x.Name)
	}
	return out
}

func refStr(n int64) string {
	if n < 0 {
		return "?"
	}
	return strconv.FormatInt(n, 10)
}

var anonVolRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

func isAnonymous(name string) bool { return anonVolRe.MatchString(name) }

func pctUsed(size, used int64) int {
	if size <= 0 {
		return 0
	}
	return int(float64(used) / float64(size) * 100.0)
}

func errText(err error) string {
	if err == nil {
		return "unknown"
	}
	return err.Error()
}

type hostFS struct {
	Source string
	Mount  string
	Size   int64
	Used   int64
	Avail  int64
	UsePct int
}

// hostFilesystems reads df for the root volume and the docker data root, which
// is where images, volumes and build cache actually land.
func hostFilesystems() ([]hostFS, error) {
	out, code, err := sshExec(20*time.Second,
		"df", "-B1", "--output=source,size,used,avail,pcent,target", "/", "/var/lib/docker")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(out) == "" {
		return nil, fmt.Errorf("df produced no output (exit %d)", code)
	}
	var rows []hostFS
	seen := map[string]bool{}
	for i, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if i == 0 {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 6 {
			continue
		}
		size, e1 := strconv.ParseInt(f[1], 10, 64)
		used, e2 := strconv.ParseInt(f[2], 10, 64)
		avail, e3 := strconv.ParseInt(f[3], 10, 64)
		if e1 != nil || e2 != nil || e3 != nil {
			continue
		}
		pct, _ := strconv.Atoi(strings.TrimSuffix(f[4], "%"))
		key := f[0] + "|" + f[5]
		if seen[key] {
			continue
		}
		seen[key] = true
		rows = append(rows, hostFS{Source: f[0], Mount: f[5], Size: size, Used: used, Avail: avail, UsePct: pct})
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("could not parse df output")
	}
	return rows, nil
}

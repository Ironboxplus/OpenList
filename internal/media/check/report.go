package check

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Status labels, in the order they are shown.
var statusLabels = []struct {
	flag  Flag
	label string
}{
	{FlagFailed, "⛔失败"},
	{FlagMissing, "⚠️缺集"},
	{FlagParse, "❓集号解析失败"},
	{FlagNoSubs, "🈚无字幕"},
	{FlagHint, "🔍提示内封但未探测"},
}

// Status is the one-line state of a result.
func (r Result) Status() string {
	var parts []string
	for _, s := range statusLabels {
		if r.Flags&s.flag != 0 {
			parts = append(parts, s.label)
		}
	}
	if len(parts) == 0 {
		return "✅完整"
	}
	return strings.Join(parts, " ")
}

// WriteReport renders the findings as markdown.
func WriteReport(w io.Writer, root string, results []Result, when time.Time) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# 媒体完整性检查\n\n- 范围：`%s`\n- 时间：%s\n- 作品数：%d\n\n", root, when.Format("2006-01-02 15:04"), len(results))

	b.WriteString("## 总览\n\n| 状态 | 作品 | 类型 | 正片 | 预期 | 缺 | 字幕 |\n|---|---|---|---|---|---|---|\n")
	for _, r := range results {
		fmt.Fprintf(&b, "| %s | %s | %s | %d | %s | %s | %s |\n",
			r.Status(), esc(r.Item.Path), r.Item.Kind, r.Features, expectCell(r), missingCount(r), subCell(r))
	}

	var detail []Result
	for _, r := range results {
		if r.Flags != 0 || len(r.Dups) > 0 || len(r.Extra) > 0 || len(r.Unparsed) > 0 || len(r.Errors) > 0 || r.ExpectNote != "" {
			detail = append(detail, r)
		}
	}
	if len(detail) > 0 {
		b.WriteString("\n## 明细\n")
		for _, r := range detail {
			writeDetail(&b, r)
		}
	}

	b.WriteString("\n## 汇总\n\n")
	counts := map[string]int{}
	complete, calls := 0, 0
	for _, r := range results {
		calls += r.Calls
		if r.Flags == 0 {
			complete++
		}
		for _, s := range statusLabels {
			if r.Flags&s.flag != 0 {
				counts[s.label]++
			}
		}
	}
	fmt.Fprintf(&b, "- ✅完整：%d / %d\n", complete, len(results))
	for _, s := range statusLabels {
		if counts[s.label] > 0 {
			fmt.Fprintf(&b, "- %s：%d\n", s.label, counts[s.label])
		}
	}
	fmt.Fprintf(&b, "- 列目录请求：%d 次\n", calls)

	var sug []Result
	for _, r := range results {
		if r.Features > 0 && len(r.Unparsed) > 0 && r.ParseRatio > 0.3 {
			sug = append(sug, r)
		}
	}
	b.WriteString("\n## 建议写 rules 的作品\n\n")
	if len(sug) == 0 {
		b.WriteString("无（没有集号解析失败率超过 30% 的作品）。\n")
	}
	for _, r := range sug {
		fmt.Fprintf(&b, "- `%s`：%d/%d 个正片读不出集号，如 `%s`\n", r.Item.Path, len(r.Unparsed), r.Features, r.Unparsed[0])
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func esc(s string) string { return strings.ReplaceAll(s, "|", "\\|") }

func expectCell(r Result) string {
	if r.Expected == 0 {
		return "-"
	}
	return fmt.Sprintf("%d (%s)", r.Expected, r.ExpectSource)
}

func missingCount(r Result) string {
	n := 0
	for _, eps := range r.Missing {
		n += len(eps)
	}
	if n == 0 {
		return "-"
	}
	return fmt.Sprint(n)
}

func subCell(r Result) string {
	s := r.Subs
	if s.Mode == "none" {
		return "(skipped)"
	}
	var parts []string
	if s.External > 0 {
		var langs []string
		for l := range s.Langs {
			langs = append(langs, l)
		}
		sort.Strings(langs)
		parts = append(parts, fmt.Sprintf("外挂%d/%d %s", s.Covered, r.Features, strings.Join(langs, "+")))
	}
	if s.Probed {
		parts = append(parts, fmt.Sprintf("内封%d轨", len(s.Tracks)))
	} else if s.Hint != "" {
		parts = append(parts, "提示内封")
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, "; ")
}

func writeDetail(b *strings.Builder, r Result) {
	fmt.Fprintf(b, "\n### %s  %s\n\n", r.Status(), r.Item.Path)
	fmt.Fprintf(b, "- 类型 %s，正片 %d 个，读出集号 %d 个，列目录 %d 次\n", r.Item.Kind, r.Features, r.Matched, r.Calls)
	if r.Expected > 0 {
		fmt.Fprintf(b, "- 预期 %d 集（来源：%s）", r.Expected, r.ExpectSource)
		if r.Unaired > 0 {
			fmt.Fprintf(b, "，另有 %d 集尚未播出，已不计", r.Unaired)
		}
		b.WriteString("\n")
	}
	if r.ExpectNote != "" {
		fmt.Fprintf(b, "- 注意：%s\n", r.ExpectNote)
	}
	var seasons []int
	for s := range r.Missing {
		seasons = append(seasons, s)
	}
	sort.Ints(seasons)
	for _, s := range seasons {
		fmt.Fprintf(b, "- 缺 S%02d：%s\n", s, compressEps(append([]int(nil), r.Missing[s]...)))
	}
	if len(r.Unparsed) > 0 {
		fmt.Fprintf(b, "- 没对上集号的正片（%d）：%s\n", len(r.Unparsed), listCap(r.Unparsed, 8))
	}
	if len(r.Dups) > 0 {
		fmt.Fprintf(b, "- 重复的集：%s\n", listCap(r.Dups, 5))
	}
	if len(r.Extra) > 0 {
		fmt.Fprintf(b, "- 超出预期的集：%s\n", listCap(r.Extra, 10))
	}
	s := r.Subs
	if s.Mode != "none" && s.Mode != "" {
		fmt.Fprintf(b, "- 字幕（模式 %s）：外挂文件 %d 个，覆盖 %d/%d 个正片", s.Mode, s.External, s.Covered, r.Features)
		if s.Orphans > 0 {
			fmt.Fprintf(b, "，%d 个对不上任何正片", s.Orphans)
		}
		b.WriteString("\n")
		if len(s.Langs) > 0 {
			var ls []string
			for l, n := range s.Langs {
				ls = append(ls, fmt.Sprintf("%s×%d", l, n))
			}
			sort.Strings(ls)
			fmt.Fprintf(b, "  - 外挂语言：%s\n", strings.Join(ls, " "))
		}
		if s.Hint != "" {
			fmt.Fprintf(b, "  - 名字提示内封字幕：`%s`\n", s.Hint)
		}
		if s.Probed {
			var ts []string
			for _, t := range s.Tracks {
				ts = append(ts, t.String())
			}
			if len(ts) == 0 {
				ts = []string{"没有字幕轨"}
			}
			fmt.Fprintf(b, "  - 探测 `%s`：%s\n", s.ProbedFile, strings.Join(ts, ", "))
		}
		if s.ProbeNote != "" {
			fmt.Fprintf(b, "  - %s\n", s.ProbeNote)
		}
		if len(s.MissingLng) > 0 {
			fmt.Fprintf(b, "  - 缺语言：%s\n", strings.Join(s.MissingLng, ", "))
		}
	}
	for _, e := range r.Errors {
		fmt.Fprintf(b, "- 错误：%s\n", e)
	}
}

func listCap(xs []string, n int) string {
	if len(xs) <= n {
		return strings.Join(xs, "; ")
	}
	return strings.Join(xs[:n], "; ") + fmt.Sprintf("; …共 %d 个", len(xs))
}

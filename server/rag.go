package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

const (
	ragRouteContext  = "rag_context"
	ragRouteNoMatch  = "rag_no_match"
	ragRouteDisabled = "rag_disabled"
)

var sectionHeaderPattern = regexp.MustCompile(`^\s*\[[^\]]+\]\s*$`)

type ragSection struct {
	Title      string
	File       string
	Text       string
	termCounts map[string]int
	norm       float64
}

type ragStore struct {
	dir      string
	sections []ragSection
	idf      map[string]float64
}

type ragResult struct {
	Route        string
	Context      string
	Sections     []string
	Files        int
	SectionCount int
	ContextChars int
}

type scoredRAGSection struct {
	section ragSection
	score   float64
}

func loadRAGStore(dir string) (*ragStore, error) {
	store := &ragStore{dir: dir, idf: map[string]float64{}}
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return store, nil
	}

	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return store, nil
		}
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("RAG path %q is not a directory", dir)
	}

	var files []string
	if err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if ext == ".md" || ext == ".txt" {
			files = append(files, path)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Strings(files)

	for _, path := range files {
		content, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		rel := path
		if relative, err := filepath.Rel(dir, path); err == nil {
			rel = relative
		}
		store.sections = append(store.sections, splitRAGSections(rel, string(content))...)
	}

	store.index()
	return store, nil
}

func (r *ragStore) Retrieve(query string, topK int, maxContextChars int, minScore float64) ragResult {
	if r == nil {
		return ragResult{Route: ragRouteDisabled}
	}

	result := ragResult{
		Route:        ragRouteNoMatch,
		Files:        r.fileCount(),
		SectionCount: len(r.sections),
	}
	if len(r.sections) == 0 {
		return result
	}
	if topK <= 0 {
		topK = 4
	}
	if maxContextChars <= 0 {
		maxContextChars = 5000
	}

	queryCounts := tokenCounts(query)
	if len(queryCounts) == 0 {
		return result
	}
	queryNorm := vectorNorm(queryCounts, r.idf)
	if queryNorm == 0 {
		return result
	}

	scored := make([]scoredRAGSection, 0, len(r.sections))
	for _, section := range r.sections {
		score := cosineScore(queryCounts, queryNorm, section.termCounts, section.norm, r.idf)
		if score >= minScore {
			scored = append(scored, scoredRAGSection{section: section, score: score})
		}
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].score == scored[j].score {
			if scored[i].section.File == scored[j].section.File {
				return scored[i].section.Title < scored[j].section.Title
			}
			return scored[i].section.File < scored[j].section.File
		}
		return scored[i].score > scored[j].score
	})

	var builder strings.Builder
	for _, item := range scored {
		if len(result.Sections) >= topK {
			break
		}
		block := fmt.Sprintf("Source: %s | %s\n%s\n\n", item.section.File, item.section.Title, item.section.Text)
		if builder.Len()+len(block) > maxContextChars {
			remaining := maxContextChars - builder.Len()
			if remaining <= 80 {
				break
			}
			block = truncateRunes(block, remaining)
		}
		builder.WriteString(block)
		result.Sections = append(result.Sections, fmt.Sprintf("%s | %s", item.section.File, item.section.Title))
		if builder.Len() >= maxContextChars {
			break
		}
	}

	result.Context = strings.TrimSpace(builder.String())
	result.ContextChars = len([]rune(result.Context))
	if result.Context != "" {
		result.Route = ragRouteContext
	}
	return result
}

func truncateRunes(text string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	return string(runes[:maxRunes])
}

func (r *ragStore) fileCount() int {
	files := map[string]struct{}{}
	for _, section := range r.sections {
		files[section.File] = struct{}{}
	}
	return len(files)
}

func (r *ragStore) Stats() (int, int) {
	if r == nil {
		return 0, 0
	}
	return r.fileCount(), len(r.sections)
}

func (r *ragStore) index() {
	docFreq := map[string]int{}
	for i := range r.sections {
		counts := tokenCounts(r.sections[i].Title + "\n" + r.sections[i].Text)
		r.sections[i].termCounts = counts
		for term := range counts {
			docFreq[term]++
		}
	}

	total := float64(len(r.sections))
	for term, count := range docFreq {
		r.idf[term] = math.Log((1+total)/(1+float64(count))) + 1
	}
	for i := range r.sections {
		r.sections[i].norm = vectorNorm(r.sections[i].termCounts, r.idf)
	}
}

func splitRAGSections(file string, content string) []ragSection {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	var sections []ragSection
	title := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
	var body []string

	flush := func() {
		text := strings.TrimSpace(strings.Join(body, "\n"))
		if text != "" {
			sections = append(sections, ragSection{
				Title: title,
				File:  filepath.ToSlash(file),
				Text:  text,
			})
		}
		body = nil
	}

	for _, line := range lines {
		if header, ok := parseRAGHeader(line); ok {
			flush()
			title = header
			continue
		}
		body = append(body, line)
	}
	flush()
	return sections
}

func parseRAGHeader(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return "", false
	}
	if strings.HasPrefix(trimmed, "#") {
		title := strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
		if title != "" {
			return title, true
		}
	}
	if sectionHeaderPattern.MatchString(trimmed) {
		return strings.TrimSpace(strings.Trim(trimmed, "[]")), true
	}
	return "", false
}

func tokenCounts(text string) map[string]int {
	counts := map[string]int{}
	for _, token := range tokenize(text) {
		counts[token]++
	}
	return counts
}

func tokenize(text string) []string {
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	tokens := make([]string, 0, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field == "" || ragStopWords[field] || len(field) < 2 {
			continue
		}
		tokens = append(tokens, field)
	}
	return tokens
}

func vectorNorm(counts map[string]int, idf map[string]float64) float64 {
	var sum float64
	for term, count := range counts {
		weight := float64(count) * idf[term]
		sum += weight * weight
	}
	return math.Sqrt(sum)
}

func cosineScore(queryCounts map[string]int, queryNorm float64, sectionCounts map[string]int, sectionNorm float64, idf map[string]float64) float64 {
	if queryNorm == 0 || sectionNorm == 0 {
		return 0
	}

	var dot float64
	for term, queryCount := range queryCounts {
		sectionCount := sectionCounts[term]
		if sectionCount == 0 {
			continue
		}
		weight := idf[term]
		dot += float64(queryCount) * weight * float64(sectionCount) * weight
	}
	return dot / (queryNorm * sectionNorm)
}

var ragStopWords = map[string]bool{
	"about": true, "after": true, "also": true, "and": true, "are": true,
	"but": true, "can": true, "for": true, "from": true, "has": true,
	"have": true, "how": true, "into": true, "its": true, "may": true,
	"not": true, "our": true, "out": true, "should": true, "that": true,
	"the": true, "their": true, "then": true, "there": true, "this": true,
	"to": true, "use": true, "was": true, "what": true, "when": true,
	"where": true, "who": true, "will": true, "with": true, "you": true,
	"your": true,
}

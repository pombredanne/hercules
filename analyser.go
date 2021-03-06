package hercules

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
	"unicode/utf8"

	"github.com/sergi/go-diff/diffmatchpatch"
	"gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/plumbing/difftree"
	"gopkg.in/src-d/go-git.v4/plumbing/object"
)

type Analyser struct {
	Repository  *git.Repository
	Granularity int
	Sampling    int
	OnProgress  func(int, int)
}

func checkClose(c io.Closer) {
	if err := c.Close(); err != nil {
		panic(err)
	}
}

func loc(file *object.Blob) (int, error) {
	reader, err := file.Reader()
	if err != nil {
		panic(err)
	}
	defer checkClose(reader)
	scanner := bufio.NewScanner(reader)
	counter := 0
	for scanner.Scan() {
		if !utf8.Valid(scanner.Bytes()) {
			return -1, errors.New("binary")
		}
		counter++
	}
	return counter, nil
}

func str(file *object.Blob) string {
	reader, err := file.Reader()
	if err != nil {
		panic(err)
	}
	defer checkClose(reader)
	buf := new(bytes.Buffer)
	buf.ReadFrom(reader)
	return buf.String()
}

func (analyser *Analyser) handleInsertion(
	change *difftree.Change, day int, status map[int]int64, files map[string]*File) {
	blob, err := analyser.Repository.Blob(change.To.TreeEntry.Hash)
	if err != nil {
		panic(err)
	}
	lines, err := loc(blob)
	if err != nil {
		return
	}
	name := change.To.Name
	file, exists := files[name]
	if exists {
		panic(fmt.Sprintf("file %s already exists", name))
	}
	file = NewFile(day, lines, status)
	files[name] = file
}

func (analyser *Analyser) handleDeletion(
	change *difftree.Change, day int, status map[int]int64, files map[string]*File) {
	blob, err := analyser.Repository.Blob(change.From.TreeEntry.Hash)
	if err != nil {
		panic(err)
	}
	lines, err := loc(blob)
	if err != nil {
		return
	}
	name := change.From.Name
	file := files[name]
	file.Update(day, 0, 0, lines)
	delete(files, name)
}

func (analyser *Analyser) handleModification(
	change *difftree.Change, day int, status map[int]int64, files map[string]*File) {
	blob_from, err := analyser.Repository.Blob(change.From.TreeEntry.Hash)
	if err != nil {
		panic(err)
	}
	blob_to, err := analyser.Repository.Blob(change.To.TreeEntry.Hash)
	if err != nil {
		panic(err)
	}
	// we are not validating UTF-8 here because for example
	// git/git 4f7770c87ce3c302e1639a7737a6d2531fe4b160 fetch-pack.c is invalid UTF-8
	str_from := str(blob_from)
	str_to := str(blob_to)
	file, exists := files[change.From.Name]
	if !exists {
		analyser.handleInsertion(change, day, status, files)
		return
	}
	// possible rename
	if change.To.Name != change.From.Name {
		analyser.handleRename(change.From.Name, change.To.Name, files)
	}
	dmp := diffmatchpatch.New()
	src, dst, _ := dmp.DiffLinesToRunes(str_from, str_to)
	if file.Len() != len(src) {
		panic(fmt.Sprintf("%s: internal integrity error src %d != %d",
			change.To.Name, len(src), file.Len()))
	}
	diffs := dmp.DiffMainRunes(src, dst, false)
	// we do not call RunesToDiffLines so the number of lines equals
	// to the rune count
	position := 0
	pending := diffmatchpatch.Diff{Text: ""}

	apply := func(edit diffmatchpatch.Diff) {
		length := utf8.RuneCountInString(edit.Text)
		if edit.Type == diffmatchpatch.DiffInsert {
			file.Update(day, position, length, 0)
			position += length
		} else {
			file.Update(day, position, 0, length)
		}
	}

	for _, edit := range diffs {
		length := utf8.RuneCountInString(edit.Text)
		func() {
			defer func() {
				r := recover()
				if r != nil {
					fmt.Fprintf(os.Stderr, "%s: internal diff error\n", change.To.Name)
					fmt.Fprint(os.Stderr, "====BEFORE====\n")
					fmt.Fprint(os.Stderr, str_from)
					fmt.Fprint(os.Stderr, "====AFTER====\n")
					fmt.Fprint(os.Stderr, str_to)
					fmt.Fprint(os.Stderr, "====END====\n")
					panic(r)
				}
			}()
			switch edit.Type {
			case diffmatchpatch.DiffEqual:
				if pending.Text != "" {
					apply(pending)
					pending.Text = ""
				}
				position += length
			case diffmatchpatch.DiffInsert:
				if pending.Text != "" {
					if pending.Type == diffmatchpatch.DiffInsert {
						panic("DiffInsert may not appear after DiffInsert")
					}
					file.Update(day, position, length, utf8.RuneCountInString(pending.Text))
					position += length
					pending.Text = ""
				} else {
					pending = edit
				}
			case diffmatchpatch.DiffDelete:
				if pending.Text != "" {
					panic("DiffDelete may not appear after DiffInsert/DiffDelete")
				}
				pending = edit
			default:
				panic(fmt.Sprintf("diff operation is not supported: %d", edit.Type))
			}
		}()
	}
	if pending.Text != "" {
		apply(pending)
		pending.Text = ""
	}
	if file.Len() != len(dst) {
		panic(fmt.Sprintf("%s: internal integrity error dst %d != %d",
			change.To.Name, len(dst), file.Len()))
	}
}

func (analyser *Analyser) handleRename(from, to string, files map[string]*File) {
	file, exists := files[from]
	if !exists {
		panic(fmt.Sprintf("file %s does not exist", from))
	}
	files[to] = file
	delete(files, from)
}

func (analyser *Analyser) Commits() []*object.Commit {
	result := []*object.Commit{}
	repository := analyser.Repository
	head, err := repository.Head()
	if err != nil {
		panic(err)
	}
	commit, err := repository.Commit(head.Hash())
	if err != nil {
		panic(err)
	}
	result = append(result, commit)
	for ; err != io.EOF; commit, err = commit.Parents().Next() {
		if err != nil {
			panic(err)
		}
		result = append(result, commit)
	}
	// reverse the order
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}
	return result
}

func (analyser *Analyser) groupStatus(status map[int]int64, day int) []int64 {
	granularity := analyser.Granularity
	if granularity == 0 {
		granularity = 1
	}
	day++
	adjust := 0
	if day%granularity < granularity-1 {
		adjust = 1
	}
	result := make([]int64, day/granularity+adjust)
	var group int64
	for i := 0; i < day; i++ {
		group += status[i]
		if i%granularity == (granularity - 1) {
			result[i/granularity] = group
			group = 0
		}
	}
	if day%granularity < granularity-1 {
		result[len(result)-1] = group
	}
	return result
}

func (analyser *Analyser) Analyse(commits []*object.Commit) [][]int64 {
	sampling := analyser.Sampling
	if sampling == 0 {
		sampling = 1
	}
	onProgress := analyser.OnProgress
	if onProgress == nil {
		onProgress = func(int, int) {}
	}

	// current daily alive number of lines; key is the number of days from the
	// beginning of the history
	status := map[int]int64{}
	// weekly snapshots of status
	statuses := [][]int64{}
	// mapping <file path> -> hercules.File
	files := map[string]*File{}

	var day0 time.Time // will be initialized in the first iteration
	var prev_tree *object.Tree = nil
	prev_day := 0

	for index, commit := range commits {
		onProgress(index, len(commits))
		tree, err := commit.Tree()
		if err != nil {
			panic(err)
		}
		if index == 0 {
			// first iteration - initialize the file objects from the tree
			day0 = commit.Author.When
			func() {
				file_iter := tree.Files()
				defer file_iter.Close()
				for {
					file, err := file_iter.Next()
					if err != nil {
						if err == io.EOF {
							break
						}
						panic(err)
					}
					lines, err := loc(&file.Blob)
					if err == nil {
						files[file.Name] = NewFile(0, lines, status)
					}
				}
			}()
		} else {
			day := int(commit.Author.When.Sub(day0).Hours() / 24)
			delta := (day / sampling) - (prev_day / sampling)
			if delta > 0 {
				prev_day = day
				gs := analyser.groupStatus(status, day)
				for i := 0; i < delta; i++ {
					statuses = append(statuses, gs)
				}
			}
			tree_diff, err := difftree.DiffTree(prev_tree, tree)
			if err != nil {
				panic(err)
			}
			for _, change := range tree_diff {
				switch change.Action {
				case difftree.Insert:
					analyser.handleInsertion(change, day, status, files)
				case difftree.Delete:
					analyser.handleDeletion(change, day, status, files)
				case difftree.Modify:
					func() {
						defer func() {
							r := recover()
							if r != nil {
								fmt.Fprintf(os.Stderr, "%s: modification error\n", commit.Hash.String())
								panic(r)
							}
						}()
						analyser.handleModification(change, day, status, files)
					}()
				default:
					panic(fmt.Sprintf("unsupported action: %d", change.Action))
				}
			}
		}
		prev_tree = tree
	}
	return statuses
}

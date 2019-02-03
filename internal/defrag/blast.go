package defrag

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/jjtimmons/defrag/config"
)

var (
	// blastnDir is a temporary directory for all blastn output
	blastnDir = ""

	// blastdbcmd is a temporary directory for all blastdbcmd output
	blastdbcmdDir = ""
)

// match is a blast "hit" in the blastdb
type match struct {
	// entry of the matched fragment in the database
	entry string

	// unique id for the match (entry name + start index % seqL). also used by fragments
	uniqueID string

	// seq of the match on the target vector
	seq string

	// start of the fragment (0-indexed)
	start int

	// end of the fragment (0-indexed)
	end int

	// the db that was BLASTed against (used later for checking off-targets in parents)
	db string

	// titles from the db. ex: year it was created
	title string

	// circular if it's a circular fragment (vector, plasmid, etc)
	circular bool

	// mismatching number of bps in the match (for primer off-targets)
	mismatching int

	// internal if the fragment doesn't have to be procured from a remote repository (eg Addgene, iGEM)
	internal bool
}

// length returns the length of the match on the target fragment
func (m *match) length() int {
	return m.end - m.start + 1 // it's inclusive
}

// copyWith returns a new match with the new start, end
func (m *match) copyWith(start, end int) match {
	return match{
		entry:       m.entry,
		seq:         m.seq,
		start:       start,
		end:         end,
		db:          m.db,
		circular:    m.circular,
		mismatching: m.mismatching,
		internal:    m.internal,
	}
}

// blastExec is a small utility object for executing BLAST
type blastExec struct {
	// the fragment we're BLASTing
	f *Frag

	// the path to the database we're BLASTing against
	db string

	// the input BLAST file
	in *os.File

	// the output BLAST file
	out *os.File

	// optional path to a FASTA file with a subject FASTA sequence
	subject string

	// internal if the db is a local/user owned list of fragments (ie free)
	internal bool

	// the percentage identity for BLAST queries
	identity float64
}

// blast the passed Frag against a set from the command line and create
// matches for those that are long enough
//
// Accepts a fragment to BLAST against, a list of dbs to BLAST it against,
// a minLength for a match, and settings around blastn location, output dir, etc
func blast(f *Frag, dbs, filters []string, minLength int, identity float64, target string) (matches []match, err error) {
	in, err := ioutil.TempFile(blastnDir, f.ID+"in-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(in.Name())

	out, err := ioutil.TempFile(blastnDir, f.ID+"out-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(out.Name())

	for _, db := range dbs {
		internal := true
		if strings.Contains(db, "addgene") || strings.Contains(db, "igem") {
			internal = false
		}

		b := &blastExec{
			f:        f,
			db:       db,
			in:       in,
			out:      out,
			internal: internal,
			identity: identity,
		}

		// make sure the db exists
		if _, err := os.Stat(db); os.IsNotExist(err) {
			return nil, fmt.Errorf("failed to find a BLAST database at %s", db)
		}

		// create the input file
		if err := b.input(); err != nil {
			return nil, fmt.Errorf("failed to write a BLAST input file at %s: %v", b.in.Name(), err)
		}

		// execute BLAST
		if err := b.run(); err != nil {
			return nil, fmt.Errorf("failed executing BLAST: %v", err)
		}

		// parse the output file to Matches against the Frag
		dbMatches, err := b.parse(filters, len(target))
		if err != nil {
			return nil, fmt.Errorf("failed to parse BLAST output: %v", err)
		}

		fmt.Printf("%d matches in %s\n", len(dbMatches), db)

		// add these matches against the growing list of matches
		matches = append(matches, dbMatches...)
	}

	// keep only "proper" arcs (non-self-contained)
	matches = filter(matches, len(f.Seq), minLength)
	if len(matches) < 1 {
		return nil, fmt.Errorf("did not find any matches for %s", f.ID)
	}

	fmt.Printf("%d matches after filtering\n", len(matches))

	// for _, m := range matches {
	// 	fmt.Printf("%s %d %d %s\n", m.entry, m.start, m.end, m.title)
	// }

	return matches, nil
}

// input creates an input file for BLAST
// return the path to the file and an error if there was one
func (b *blastExec) input() error {
	// create the query sequence file.
	// add the sequence to itself because it's circular
	// and we want to find matches across the zero-index.
	file := fmt.Sprintf(">%s\n%s\n", b.f.ID, b.f.Seq+b.f.Seq)
	if _, err := b.in.WriteString(file); err != nil {
		return err
	}
	return nil
}

// run calls the external blastn binary on the input library
func (b *blastExec) run() (err error) {
	threads := runtime.NumCPU() - 1
	if threads < 1 {
		threads = 1
	}

	// create the blast command
	// https://www.ncbi.nlm.nih.gov/books/NBK279682/
	blastCmd := exec.Command(
		"blastn",
		"-task", "blastn",
		"-db", b.db,
		"-query", b.in.Name(),
		"-out", b.out.Name(),
		"-outfmt", "7 sseqid qstart qend sstart send sseq mismatch stitle",
		"-perc_identity", fmt.Sprintf("%f", b.identity),
		"-num_threads", strconv.Itoa(threads),
		"-max_target_seqs", "500", // default is 500
	)

	// execute BLAST and wait on it to finish
	if output, err := blastCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to execute blastn against %s: %v: %s", b.db, err, string(output))
	}
	return
}

// parse reads the output file into Matches on the Frag
// returns a slice of Matches for the blasted fragment
func (b *blastExec) parse(filters []string, targetLength int) (matches []match, err error) {
	// read in the results
	file, err := ioutil.ReadFile(b.out.Name())
	if err != nil {
		return
	}
	fileS := string(file)

	// fmt.Printf("%+v", filters)

	// read it into Matches
	var ms []match
	for _, line := range strings.Split(fileS, "\n") {
		// comment lines start with a #
		if strings.HasPrefix(line, "#") {
			continue
		}

		// split on white space
		cols := strings.Fields(line)
		if len(cols) < 6 {
			continue
		}

		// the full entry line, (id, direction, etc)
		entry := strings.Replace(cols[0], ">", "", -1)
		start, _ := strconv.Atoi(cols[1])
		end, _ := strconv.Atoi(cols[2])
		seq := cols[5]
		mm, _ := strconv.Atoi(cols[6])
		titles := cols[7] // salltitles, ex: "fwd terminator 2011"

		// direction not guarenteed
		if start > end {
			start, end = end, start
		}

		// filter on titles
		matchesFilter := false
		search := strings.ToUpper(entry + titles)
		for _, f := range filters {
			if strings.Contains(search, f) {
				matchesFilter = true
				break
			}
		}
		if matchesFilter {
			continue // has been filtered out because of "filter" CLI flag
		}

		// create and append the new match
		ms = append(ms, match{
			entry:       entry,
			uniqueID:    entry + strconv.Itoa(start%targetLength),
			seq:         strings.Replace(seq, "-", "", -1),
			start:       start - 1, // convert 1-based numbers to 0-based
			end:         end - 1,
			circular:    strings.Contains(titles, "circular"),
			mismatching: mm,
			internal:    b.internal,
			db:          b.db, // store for checking off-targets later
			title:       titles,
		})
	}

	return ms, nil
}

// filter "proper-izes" the matches from BLAST
//
// TODO: filter further here, can remove external matches that are
// entirely contained by internal matches but am not doing that here
//
// proper-izing fragment matches means removing those that are completely
// self-contained in other fragments: the larger of the available fragments
// will be the better one, since it covers a greater region and will almost
// always be preferable to the smaller one
//
// Circular-arc graph: https://en.wikipedia.org/wiki/Circular-arc_graph
//
// also remove small fragments here that are too small to be useful during assembly
func filter(matches []match, targetLength, minSize int) (properized []match) {
	properized = []match{}

	// remove fragments that are shorter the minimum cut off size
	// separate the internal and external fragments. the internal
	// ones should not be removed just if they're self-contained
	// in another, because they may be cheaper to assemble
	var internal []match
	var external []match
	for _, m := range matches {
		if m.length() < minSize {
			continue // too short
		}

		if m.internal {
			internal = append(internal, m)
		} else {
			external = append(external, m)
		}
	}

	// create properized matches (non-self contained)
	properized = append(properize(internal), properize(external)...)

	// because we properized the matches, we may have removed a match from the
	// start or the end. right now, a match showing up twice in the vector
	// is how we circularize, so have to add back matches to the start or end
	matchCount := make(map[string]int)
	for _, m := range properized {
		if _, counted := matchCount[m.uniqueID]; counted {
			matchCount[m.uniqueID]++
		} else {
			matchCount[m.uniqueID] = 1
		}
	}

	// add back copied matches for those that only show up once
	copiedMatches := []match{}
	for _, m := range properized {
		if count := matchCount[m.uniqueID]; count == 2 {
			continue
		}

		// first half of the queried seq range (2 seq lengths)
		if m.end < targetLength {
			copiedMatches = append(copiedMatches, m.copyWith(m.start+targetLength, m.end+targetLength))
		} else if m.start > targetLength {
			copiedMatches = append(copiedMatches, m.copyWith(m.start-targetLength, m.end-targetLength))
		}
	}

	// sort again now that we added copied matches
	properized = append(properized, copiedMatches...)
	sortMatches(properized)

	return properized
}

// properize remove matches that are entirely contained within others
func properize(matches []match) (properized []match) {
	sortMatches(matches)

	// only include those that aren't encompassed by the one before it
	for _, m := range matches {
		lastMatch := len(properized) - 1
		if lastMatch < 0 || m.end > properized[lastMatch].end {
			properized = append(properized, m)
		}
	}

	return
}

// sortMatches sorts matches by their start index
// for fragments with equivelant starting indexes, put the larger one first
func sortMatches(matches []match) {
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].start != matches[j].start {
			return matches[i].start < matches[j].start
		} else if matches[i].length() != matches[j].length() {
			return matches[i].length() > matches[j].length()
		}
		return matches[i].entry > matches[j].entry
	})
}

// queryDatabases is for finding a fragment/vector with the entry name in one of the dbs
func queryDatabases(entry string, dbs []string) (f Frag, err error) {
	// first try to get the entry out of a local file
	if frags, err := read(entry); err == nil && len(frags) > 0 {
		return frags[0], nil // it was a local file
	}

	// move through each db and see if it contains the entry
	for _, db := range dbs {
		// if outFile is defined here we managed to query it from the db
		outFile, err := blastdbcmd(entry, db)
		if err == nil && outFile.Name() != "" {
			defer os.Remove(outFile.Name())
			frags, err := read(outFile.Name())
			return frags[0], err
		}
	}

	dbMessage := strings.Join(dbs, "\n")
	return Frag{}, fmt.Errorf("failed to find %s in any of:\n\t%s", entry, dbMessage)
}

// seqMismatch queries for any mismatching primer locations in the parent sequence
// unlike parentMismatch, it doesn't first find the parent fragment from the db it came from
// the sequence is passed directly as parentSeq
func seqMismatch(primers []Primer, parentID, parentSeq string, conf *config.Config) (wasMismatch bool, m match, err error) {
	parentFile, err := ioutil.TempFile(blastnDir, parentID+".parent-*")
	if err != nil {
		return false, match{}, err
	}
	defer os.Remove(parentFile.Name())

	inContent := fmt.Sprintf(">%s\n%s\n", parentID, parentSeq)
	if _, err = parentFile.WriteString(inContent); err != nil {
		return false, m, fmt.Errorf("failed to write primer sequence to query FASTA file: %v", err)
	}

	// check each primer for mismatches
	for _, primer := range primers {
		wasMismatch, m, err = mismatch(primer.Seq, parentFile, conf)
		if wasMismatch || err != nil {
			return
		}
	}

	return false, match{}, nil
}

// parentMismatch both searches for a the parent fragment in its source DB and queries for
// any mismatches in the seq before returning
func parentMismatch(primers []Primer, parent, db string, conf *config.Config) (wasMismatch bool, m match, err error) {
	// try and query for the parent in the source DB and write to a file
	parentFile, err := blastdbcmd(parent, db)

	// ugly check here for whether we just failed to get the parent entry from a db
	// which isn't a huge deal (shouldn't be flagged as a mismatch)
	// this is similar to what io.IsNotExist does
	if err != nil {
		if strings.Contains(err.Error(), "failed to query") {
			fmt.Println(err) // just write the error
			// TODO: if we fail to find the parent, query the fullSeq as it was sent
			return false, match{}, nil
		}
		return false, match{}, err
	}

	// check each primer for mismatches
	if parentFile.Name() != "" {
		defer os.Remove(parentFile.Name())

		for _, primer := range primers {
			wasMismatch, m, err = mismatch(primer.Seq, parentFile, conf)
			if wasMismatch || err != nil {
				return
			}
		}
	}

	return
}

// blastdbcmd queries a fragment/vector by its FASTA entry name (entry) and writes the
// results to a temporary file (to be BLAST'ed against)
//
// entry here is the ID that's associated with the fragment in its source DB (db)
func blastdbcmd(entry, db string) (output *os.File, err error) {
	// path to the entry batch file to hold the entry accession
	entryFile, err := ioutil.TempFile(blastdbcmdDir, ".in-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(entryFile.Name())

	// path to the output sequence file from querying the entry's sequence from the BLAST db
	output, err = ioutil.TempFile(blastdbcmdDir, ".out-*")
	if err != nil {
		return nil, err
	}

	// write entry to file
	// this was a 2-day issue I couldn't resolve...
	// I was using the "-entry" flag on exec.Command, but have since
	// switched to the simpler -entry_batch command (on a file) that resolves the issue
	if _, err := entryFile.WriteString(entry); err != nil {
		return nil, fmt.Errorf("failed to write blastdbcmd entry file at %s: %v", entryFile.Name(), err)
	}

	// make a blastdbcmd command (for querying a DB, very different from blastn)
	queryCmd := exec.Command(
		"blastdbcmd",
		"-db", db,
		"-dbtype", "nucl",
		"-entry_batch", entryFile.Name(),
		"-out", output.Name(),
		"-outfmt", "%f ", // fasta format
	)

	// execute
	if _, err := queryCmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("warning: failed to query %s from %s\n\t%s", entry, db, err.Error())
	}

	// read in the results as a fragment and return just the seq
	fragments, err := read(output.Name())
	if err == nil && len(fragments) >= 1 {
		return output, nil
	}

	return nil, fmt.Errorf("warning: failed to query %s from %s", entry, db)
}

// mismatch finds mismatching sequences between the query sequence and
// the parent sequence (in the parent file)
//
// The fragment to query against is stored in parentFile
func mismatch(primer string, parentFile *os.File, c *config.Config) (wasMismatch bool, m match, err error) {
	// path to the entry batch file to hold the entry accession
	in, err := ioutil.TempFile(blastnDir, ".primer.in")
	if err != nil {
		return false, match{}, err
	}
	defer os.Remove(in.Name())

	// path to the output sequence file from querying the entry's sequence from the BLAST db
	out, err := ioutil.TempFile(blastnDir, ".primer.out")
	if err != nil {
		return false, match{}, err
	}
	defer os.Remove(out.Name())

	// create blast input file
	inContent := fmt.Sprintf(">primer\n%s\n", primer)
	if _, err = in.WriteString(inContent); err != nil {
		return false, m, fmt.Errorf("failed to write primer sequence to query FASTA file: %v", err)
	}

	// blast the query sequence against the parentFile sequence
	b := blastExec{
		in:      in,
		out:     out,
		subject: parentFile.Name(),
	}

	// execute blast
	if err = b.runAgainst(); err != nil {
		return false, m, fmt.Errorf("failed to run blast against parent: %v", err)
	}

	// get the BLAST matches
	matches, err := b.parse([]string{}, 1)
	if err != nil {
		return false, match{}, fmt.Errorf("failed to parse matches from %s: %v", out.Name(), err)
	}

	// parse the results and check whether any are cause for concern (by Tm)
	primerCount := 1 // number of times we expect to see the primer itself
	parentFileContents, err := ioutil.ReadFile(parentFile.Name())
	if err != nil {
		return false, match{}, err
	}

	if strings.Contains(string(parentFileContents), "circular") {
		// if the match is against a circular fragment, we expect to see the primer's binding location
		// twice because circular fragments' sequences are doubled in the DBs
		// TODO: one exception here is if the primer is on a range that crosses the zero index
		primerCount++
	}

	for _, m := range matches {
		if isMismatch(m, c) {
			primerCount--
		}

		if primerCount < 0 {
			return true, m, nil
		}
	}

	return false, match{}, nil
}

// runs blast on the query file against another subject file (rather than blastdb)
func (b *blastExec) runAgainst() (err error) {
	// create the blast command
	// https://www.ncbi.nlm.nih.gov/books/NBK279682/
	blastCmd := exec.Command(
		"blastn",
		"-task", "blastn",
		"-query", b.in.Name(),
		"-subject", b.subject,
		"-out", b.out.Name(),
		"-outfmt", "7 sseqid qstart qend sstart send sseq mismatch salltitles",
	)

	// execute BLAST and wait on it to finish
	if output, err := blastCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to execute blastn against %s: %v: %s", b.subject, err, string(output))
	}

	return
}

// isMismatch reutns whether the match constitutes a mismatch
// between it and the would be primer sequence
//
// source: http://depts.washington.edu/bakerpg/primertemp/
//
// The equation used for the melting temperature is:
// Tm = 81.5 + 0.41(%GC) - 675/N - % mismatch, where N = total number of bases.
func isMismatch(m match, c *config.Config) bool {
	primer := strings.ToUpper(m.seq)
	primerL := float64(len(primer))

	noA := strings.Replace(primer, "a", "", -1)
	noT := strings.Replace(noA, "t", "", -1)
	gcPerc := float64(len(noT)) / primerL
	tmNoMismatch := 81.5 + 0.41*gcPerc - 675/float64(len(primer))
	tmWithMismatch := tmNoMismatch - float64(m.mismatching)/primerL

	return tmWithMismatch > c.PCRMaxOfftargetTm
}

func init() {
	var err error

	blastnDir, err = ioutil.TempDir("", "blastn")
	if err != nil {
		stderr.Fatal(err)
	}

	blastdbcmdDir, err = ioutil.TempDir("", "blastdbcmd")
	if err != nil {
		stderr.Fatal(err)
	}
}

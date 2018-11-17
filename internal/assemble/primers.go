package assemble

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jjtimmons/decvec/internal/blast"
	"github.com/jjtimmons/decvec/internal/dvec"
)

// p3Exec is a utility struct for executing primer3 to create primers for a part
type p3Exec struct {
	// node that we're trying to create primers for
	n *node

	// the node before this one
	last node

	// the node after this one
	next node

	// the target sequence
	target string

	// input file
	in string

	// output file
	out string

	// path to primer3 executable
	p3Path string

	// path to primer3 config folder (with trailing separator)
	p3Conf string

	// path to the primer3 io output
	p3Dir string
}

// newP3Exec creates a p3Exec from a fragment
func newP3Exec(last, this, next node, target string) p3Exec {
	p3Path := filepath.Join(conf.Root, "vendor", "primer3-2.4.0", "src", "primer3_core")
	p3Conf := filepath.Join(conf.Root, "vendor", "primer3-2.4.0", "src", "primer3_config") + "/"
	p3Dir := filepath.Join(conf.Root, "bin", "primer3")

	_, err := os.Stat(p3Path)
	if err != nil {
		log.Fatalf("Failed to locate primer3 executable: %v", err)
	}

	_, err = os.Stat(p3Conf)
	if err != nil {
		log.Fatalf("Failed to locate primer3 config folder: %v", err)
	}

	err = os.MkdirAll(p3Dir, os.ModePerm)
	if err != nil {
		log.Fatalf("Failed to create a primer3 outut dir: %v", err)
	}

	return p3Exec{
		n:      &this,
		last:   last,
		next:   next,
		target: strings.ToUpper(target),
		in:     path.Join(p3Dir, this.id+".in"),
		out:    path.Join(p3Dir, this.id+".out"),
		p3Path: p3Path,
		p3Conf: p3Conf,
		p3Dir:  p3Dir,
	}
}

// primers creates primers against a node and return an error if
//	1. the primers have an unacceptably high primer3 penalty score
//	2. the primers have off-targets in their parent source
func primers(last, this, next node, vec string) (primers []dvec.Primer, err error) {
	exec := newP3Exec(last, this, next, vec)

	// make input file, figure out how to create primers that share homology
	// with neighboring nodes
	if err = exec.input(); err != nil {
		return
	}

	if err = exec.run(); err != nil {
		return
	}

	if primers, err = exec.parse(); err != nil {
		return
	}

	// 1. check for whether the primers have too have a pair penalty score
	if primers[0].PairPenalty > conf.PCR.P3MaxPenalty {
		return nil, fmt.Errorf(
			"Primers have pair primer3 penalty score of %f, should be less than %f:\n%+v\n%+v",
			primers[0].PairPenalty,
			conf.PCR.P3MaxPenalty,
			primers[0],
			primers[1],
		)
	}

	// 2. check for whether either of the primers have an off-target/mismatch
	for _, primer := range primers {
		// the node's id is the same as the entry ID in the database
		mismatchExists, mismatch, err := blast.Mismatch(primer.Seq, this.id, conf.DB)

		if err != nil {
			return nil, err
		}

		if mismatchExists {
			return nil, fmt.Errorf(
				"Found a mismatching sequence, %s, against the primer %s",
				mismatch.Seq,
				primer.Seq,
			)
		}
	}

	return
}

// input makes the primer3 input settings file and writes it to the filesystem
//
// the primers on this node should account for creating homology
// against the last node and the next node if there isn't enough
// existing homology to begin with (the two nodes should share ~50/50)
func (p *p3Exec) input() error {
	// calc the # of bp this node shares with another
	bpToShare := func(left, right node) (bpToAdd int) {
		// calc the # of bp the left node is responsible with the right one
		bpToAdd = 0
		if synthDist := left.synthDist(right); synthDist == 0 {
			// we're not going to synth our way here, check if there's already enough homology
			if bpDist := left.distTo(right); bpDist > -(conf.Fragments.MinHomology) {
				// this node will add half the homology to the last fragment
				// ex: 5 bp distance leads to 2.5bp + ~10bp additonal
				// ex: -10bp distance leads to ~0 bp additional:
				//	other node is responsible for all of it
				bpToAdd = bpDist + (conf.Fragments.MinHomology / 2)
			}
		}
		return
	}

	// calc the bps to add on the left and right side of this node
	addLeft := bpToShare(p.last, *p.n)
	addRight := bpToShare(*p.n, p.next)
	maxAdded := addLeft
	if addRight > maxAdded {
		maxAdded = addRight
	}

	// the node's range plus the additional bp added because of adding homology
	start := p.n.start - addLeft
	length := p.n.end - start + 1
	length += addRight

	// sizes to make the primers and target size (min, opt, and max)
	targetSizeMin := p.n.end - p.n.start
	targetSizeMax := targetSizeMin
	primerMin := 18 // defaults
	primerOpt := 20
	primerMax := 23
	if maxAdded > 0 {
		maxAdded += 2

		// we can't exceed 36 here
		if maxAdded > 36-primerMax {
			maxAdded = 36 - primerMax
		}

		// targetSizeMin -= 2
		// targetSizeMax += 2
		primerMin += maxAdded
		primerOpt += maxAdded
		primerMax += maxAdded
	}
	fmt.Printf("%s %d %d %d %d %d\n", p.n.id, targetSizeMin, start, length, maxAdded, primerOpt)

	// see primer3 manual or /vendor/primer3-2.4.0/settings_files/p3_th_settings.txt
	// TODO: check whether optimal primer sizes can be set for left and right separately
	settings := map[string]string{
		"PRIMER_THERMODYNAMIC_PARAMETERS_PATH": p.p3Conf,
		"PRIMER_NUM_RETURN":                    "1",
		"PRIMER_TASK":                          "pick_cloning_primers",
		"PRIMER_PICK_ANYWAY":                   "1",
		"SEQUENCE_TEMPLATE":                    p.target,
		"SEQUENCE_INCLUDED_REGION":             fmt.Sprintf("%d,%d", start, length),
		"PRIMER_PRODUCT_SIZE_RANGE":            fmt.Sprintf("%d-%d", targetSizeMin, targetSizeMax),
		"PRIMER_MIN_SIZE":                      strconv.Itoa(primerMin), // default 18
		"PRIMER_OPT_SIZE":                      strconv.Itoa(primerOpt), // 20
		"PRIMER_MAX_SIZE":                      strconv.Itoa(primerMax), // 23
	}

	var fileContents string
	for key, val := range settings {
		fileContents += fmt.Sprintf("%s=%s\n", key, val)
	}
	fileContents += "=" // required at file's end

	if err := ioutil.WriteFile(p.in, []byte(fileContents), 0666); err != nil {
		return fmt.Errorf("Failed to create primer3 input file %v: ", err)
	}

	return nil
}

// run the primer3 executable on the input file
func (p *p3Exec) run() error {
	p3Cmd := exec.Command(
		p.p3Path,
		p.in,
		"-output", p.out,
		"-strict_tags",
	)

	// execute primer3 and wait on it to finish
	if output, err := p3Cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("Failed to execute primer3: %s: %v", string(output), err)
	}
	return nil
}

// parse the output into primers for the part
func (p *p3Exec) parse() (primers []dvec.Primer, err error) {
	file, err := ioutil.ReadFile(p.out)
	if err != nil {
		return nil, err
	}
	fileS := string(file)

	// read in results into map, they're all 1:1
	results := make(map[string]string)
	for _, line := range strings.Split(fileS, "\n") {
		keyVal := strings.Split(line, "=")
		if len(keyVal) > 1 {
			results[strings.TrimSpace(keyVal[0])] = strings.TrimSpace(keyVal[1])
		}
	}

	if p3Error := results["PRIMER_ERROR"]; p3Error != "" {
		return nil, fmt.Errorf("Failed to execute primer3: %s", p3Error)
	}

	// read in a single primer from the output string file
	// side is either "LEFT" or "RIGHT"
	parsePrimer := func(side string) dvec.Primer {
		seq := results[fmt.Sprintf("PRIMER_%s_0_SEQUENCE", side)]
		tm := results[fmt.Sprintf("PRIMER_%s_0_TM", side)]
		gc := results[fmt.Sprintf("PRIMER_%s_0_GC_PERCENT", side)]
		penalty := results[fmt.Sprintf("PRIMER_%s_0_PENALTY", side)]
		pairPenalty := results["PRIMER_PAIR_0_PENALTY"]

		tmfloat, _ := strconv.ParseFloat(tm, 32)
		gcfloat, _ := strconv.ParseFloat(gc, 32)
		penaltyfloat, _ := strconv.ParseFloat(penalty, 32)
		pairfloat, _ := strconv.ParseFloat(pairPenalty, 32)

		return dvec.Primer{
			Seq:         seq,
			Strand:      side == "LEFT",
			Tm:          float32(tmfloat),
			GC:          float32(gcfloat),
			Penalty:     float32(penaltyfloat),
			PairPenalty: float32(pairfloat),
		}
	}

	return []dvec.Primer{
		parsePrimer("LEFT"),
		parsePrimer("RIGHT"),
	}, nil
}

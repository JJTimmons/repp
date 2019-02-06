package defrag

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/jjtimmons/defrag/config"
)

// primer3Dir is a temporary directory holding primer3 input and output
var primer3Dir = ""

// primer3 is a utility struct for executing primer3 to create primers for a part
type primer3 struct {
	// Frag that we're trying to create primers for
	f *Frag

	// the Frag before this one
	last *Frag

	// the Frag after this one
	next *Frag

	// the target sequence
	seq string

	// input file
	in *os.File

	// output file
	out *os.File

	// path to primer3 executable
	primer3Path string

	// path to primer3 config folder (with trailing separator)
	primer3ConfDir string

	// path to the primer3 io output
	primer3Dir string
}

// newPrimer3 creates a primer3 struct from a fragment
func newPrimer3(last, this, next *Frag, seq string, conf *config.Config) primer3 {
	in, _ := ioutil.TempFile(primer3Dir, this.ID+".in-*")
	out, _ := ioutil.TempFile(primer3Dir, this.ID+".out-*")

	return primer3{
		f:              this,
		last:           last,
		next:           next,
		seq:            strings.ToUpper(seq),
		in:             in,
		out:            out,
		primer3Path:    "primer3_core",
		primer3ConfDir: conf.Primer3Config,
		primer3Dir:     primer3Dir,
	}
}

// input makes a primer3 input settings file and writes it to the filesystem
//
// the primers on this Frag should account for creating homology
// against the last Frag and the next Frag if there isn't enough
// existing homology to begin with (the two nodes should share ~50/50)
//
// returning the number of bp that have to be artifically added to the left and right primers
func (p *primer3) input(minHomology, maxHomology, maxEmbedLength, minLength int) (bpAddLeft, bpAddRight int, err error) {
	// adjust the Frag's start and end index in the event that there's too much homology
	// with the neighboring fragment
	p.shrink(p.last, p.f, p.next, maxHomology, minLength) // could skip passing as a param, but this is a bit easier to test imo

	// calc the bps to add on the left and right side of this Frag
	addLeft := p.bpToAdd(p.last, p.f)
	addRight := p.bpToAdd(p.f, p.next)
	growPrimers := addLeft
	if growPrimers < addRight {
		growPrimers = addRight
	}

	start := p.f.start
	length := p.f.end - start

	// determine whether we need to add additional bp to the primers. if there's too much to
	// add, or if we're adding largely different amounts to the FWD and REV primer, we set
	// the number of bp to add as bpAddLeft and bpAddRight and do so after creating primers
	// that anneal to the template
	primerDiff := addLeft - addRight
	if primerDiff > 5 || primerDiff < 5 {
		// if one sides has a lot to add but the other doesn't, don't increase
		// the primer generation size in primer3. will instead concat the sequence on later
		// because we do not want to throw off the annealing temp for the primers
		bpAddLeft = addLeft
		bpAddRight = addRight
		growPrimers = 0
	} else if growPrimers > 36-26 {
		// we can't exceed 36 bp here (primer3 upper-limit), just create primers for the portion that
		// anneals to the template and add the other portion/seqs on later (in mutateNodePrimers)
		bpAddLeft = addLeft
		bpAddRight = addRight
		growPrimers = 0
	} else {
		start -= addLeft
		length = p.f.end - start
		length += addRight
	}

	// sizes to make the primers and target size (min, opt, and max)
	primerMin := 18 + growPrimers // defaults to 18
	primerOpt := 20 + growPrimers
	primerMax := 26 + growPrimers // defaults to 23

	// check whether we have wiggle room on the left or right hand sides to move the
	// primers inward (let primer3 pick better primers)
	//
	// also adjust start and length in case there's TOO large an overhang and we need
	// to trim it in one direction or the other
	leftBuffer := p.buffer(p.last.distTo(p.f), minHomology, maxEmbedLength)
	rightBuffer := p.buffer(p.f.distTo(p.next), minHomology, maxEmbedLength)

	// create the settings map from all instructions
	file, err := p.settings(
		p.seq,
		p.primer3ConfDir,
		start,
		length,
		primerMin,
		primerOpt,
		primerMax,
		leftBuffer,
		rightBuffer,
	)
	if err != nil {
		return 0, 0, err
	}

	if _, err := p.in.Write(file); err != nil {
		return 0, 0, fmt.Errorf("failed to write primer3 input file %v: ", err)
	}
	return
}

// shrink adjusts the start and end of a Frag in the scenario where
// it excessively overlaps a neighboring fragment. For example, if there's
// 700bp of overlap, this will trim it back so we just PCR a subselection of
// the Frag and keep the overlap beneath the upper limit
// TODO: consider giving minLength for PCR fragments precedent, this may subvert that rule
func (p *primer3) shrink(last, f, next *Frag, maxHomology int, minLength int) *Frag {
	var shiftInLeft int
	var shiftInRight int

	if distLeft := last.distTo(f); distLeft < -maxHomology {
		// there's too much homology on the left side, we should move the Frag's start inward
		shiftInLeft = (-distLeft) - maxHomology
	}

	if distRight := f.distTo(next); distRight < -maxHomology {
		// there's too much homology on the right side, we should move the Frag's end inward
		shiftInRight = (-distRight) - maxHomology
	}

	// make sure the fragment doesn't become less than the minimum length
	if (f.end-shiftInRight)-(f.start+shiftInLeft) > minLength {

		// update the seq to slice for the new modified range
		f.start += shiftInLeft
		f.end -= shiftInRight

		if f.Seq != "" {
			f.Seq = f.Seq[shiftInLeft : len(f.Seq)-shiftInRight]
		}
	}

	return f
}

// bpToAdd calculates the number of bp to add the end of a left Frag to create a junction
// with the rightmost Frag
func (p *primer3) bpToAdd(left, right *Frag) int {
	if !left.overlapsViaPCR(right) {
		return 0 // we're going to synthesize there, don't add bp via PCR
	}

	if left.overlapsViaHomology(right) {
		return 0 // there is already enough overlap via PCR
	}

	// we're not going to synth our way here, check that there's already enough homology
	minHomology := left.conf.FragmentsMinHomology
	if bpDist := left.distTo(right); bpDist > -minHomology {
		// this Frag will add half the homology to the last fragment
		// eg: 5 bp distance leads to 2.5bp + ~10bp additonal
		// eg: -10bp distance leads to ~0 bp additional:
		// 		other Frag is responsible for all of it
		return bpDist + (minHomology / 2)
	}

	return 0
}

// buffer takes the dist from a one fragment to another and
// returns the length of the "buffer" in which the primers can be optimized (let primer3 pick)
//
// dist is positive if there's a gap between the start/end of a fragment and the start/end of
// the other and negative if they overlap
func (p *primer3) buffer(dist, minHomology, maxEmbedLength int) (buffer int) {
	if dist > maxEmbedLength {
		// we'll synthesize because the gap is so large, add 100bp of buffer
		return 100 // TODO: move "100" to settings
	}

	if dist < -minHomology {
		// there's enough additonal overlap that we can move this FWD primer inwards
		// but only enough to ensure that there's still minHomology bp overlap
		// and only enough so we leave the neighbor space for primer optimization too
		return (-dist - minHomology) / 2
	}

	return 0
}

// settingsMap returns a new settings map for the primer3 config files
// can either use pick_cloning_primers mode, if the start and end primers' locations
// are fixed, or pick_primer_list mode if we're letting the primers shift and allowing
// primer3 to pick the best ones. One side may be free to move and the other not
func (p *primer3) settings(
	seq, p3conf string,
	start, length, primerMin, primerOpt, primerMax int,
	leftBuffer, rightBuffer int) (file []byte, err error) {
	// see primer3 manual or /vendor/primer3-2.4.0/settings_files/p3_th_settings.txt
	settings := map[string]string{
		"SEQUENCE_ID":                          p.f.ID,
		"PRIMER_THERMODYNAMIC_PARAMETERS_PATH": p3conf,
		"PRIMER_NUM_RETURN":                    "1",
		"PRIMER_PICK_ANYWAY":                   "1",
		"SEQUENCE_TEMPLATE":                    seq + seq + seq,         // triple sequence
		"PRIMER_MIN_SIZE":                      strconv.Itoa(primerMin), // default 18
		"PRIMER_OPT_SIZE":                      strconv.Itoa(primerOpt),
		"PRIMER_MAX_SIZE":                      strconv.Itoa(primerMax),
		"PRIMER_EXPLAIN_FLAG":                  "1",
		"PRIMER_MIN_TM":                        "47.0",                                              // defaults to 57.0
		"PRIMER_MAX_TM":                        "73.0",                                              // defaults to 63.0
		"PRIMER_MAX_HAIRPIN_TH":                fmt.Sprintf("%f", p.f.conf.FragmentsMaxHairpinMelt), // defaults to 47.0
		"PRIMER_MAX_POLY_X":                    "7",                                                 // defaults to 5
		"PRIMER_PAIR_MAX_COMPL_ANY":            "13.0",                                              // defaults to 8.00
	}

	start += len(seq) // move to one seq length further in the vector seq (get off left edge)

	// if there is room to optimize, we let primer3 pick the best primers available in the space
	// http://primer3.sourceforge.net/primer3_manual.htm#SEQUENCE_PRIMER_PAIR_OK_REGION_LIST
	if leftBuffer > 0 || rightBuffer > 0 {
		// V-ls  v-le               v-rs   v-re
		// ---------------------------------
		leftEnd := start + leftBuffer + primerMax
		rightStart := start + length - rightBuffer - primerMax
		excludeLength := rightStart - leftEnd

		settings["PRIMER_TASK"] = "generic"
		settings["PRIMER_PICK_LEFT_PRIMER"] = "1"
		settings["PRIMER_PICK_INTERNAL_OLIGO"] = "0"
		settings["PRIMER_PICK_RIGHT_PRIMER"] = "1"

		if excludeLength >= 0 {
			if excludeLength < primerMax {
				return nil, fmt.Errorf("pcr minimum size, %d, smaller than max primer size, %d", excludeLength, primerMax)
			}

			// ugly undoing of the above in case only one side has buffer
			if leftBuffer == 0 {
				settings["SEQUENCE_FORCE_LEFT_START"] = strconv.Itoa(start)
			} else if rightBuffer == 0 {
				settings["SEQUENCE_FORCE_RIGHT_START"] = strconv.Itoa(start + length - 1)
			}
			settings["PRIMER_PRODUCT_SIZE_RANGE"] = fmt.Sprintf("%d-%d", excludeLength, length)
			settings["SEQUENCE_PRIMER_PAIR_OK_REGION_LIST"] = fmt.Sprintf("%d,%d,%d,%d", start, leftBuffer+primerMax, rightStart, rightBuffer+primerMax)
		}
	}

	if settings["SEQUENCE_PRIMER_PAIR_OK_REGION_LIST"] == "" {
		// otherwise force the start and end of the PCR range
		settings["PRIMER_TASK"] = "pick_cloning_primers"
		settings["SEQUENCE_INCLUDED_REGION"] = fmt.Sprintf("%d,%d", start, length)
		settings["PRIMER_PRODUCT_SIZE_RANGE"] = fmt.Sprintf("%d-%d", length-1, length)
	}

	var fileBuffer bytes.Buffer
	for key, val := range settings {
		fmt.Fprintf(&fileBuffer, "%s=%s\n", key, val)
	}
	fileBuffer.WriteString("=") // required at file's end

	return fileBuffer.Bytes(), nil
}

// run the primer3 executable against the input file
func (p *primer3) run() (err error) {
	p3Cmd := exec.Command(
		p.primer3Path,
		p.in.Name(),
		"-output", p.out.Name(),
		"-strict_tags",
	)

	// execute primer3 and wait on it to finish
	if output, err := p3Cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to execute primer3 on input file %s: %s: %v", p.in.Name(), string(output), err)
	}

	return
}

// parse the output into primers
//
// target is the target sequence we're building for. We need it to modulo the primer ranges
func (p *primer3) parse(target string) (err error) {
	fileBytes, err := ioutil.ReadFile(p.out.Name())
	if err != nil {
		return
	}
	file := string(fileBytes)

	// read in results into map, they're all 1:1
	results := make(map[string]string)
	for _, line := range strings.Split(file, "\n") {
		keyVal := strings.Split(line, "=")
		if len(keyVal) > 1 {
			results[strings.TrimSpace(keyVal[0])] = strings.TrimSpace(keyVal[1])
		}
	}

	if p3Warnings := results["PRIMER_WARNING"]; p3Warnings != "" {
		return fmt.Errorf("warnings executing primer3: %s", p3Warnings)
	}

	if p3Error := results["PRIMER_ERROR"]; p3Error != "" {
		fmt.Println(file)
		return fmt.Errorf("failed to execute primer3: %s", p3Error)
	}

	if numPairs := results["PRIMER_PAIR_NUM_RETURNED"]; numPairs == "0" {
		fmt.Println(file)
		return fmt.Errorf("failed to create primers")
	}

	// read in a single primer from the output string file
	// side is either "LEFT" or "RIGHT"
	parsePrimer := func(side string) Primer {
		seq := results[fmt.Sprintf("PRIMER_%s_0_SEQUENCE", side)]
		tm := results[fmt.Sprintf("PRIMER_%s_0_TM", side)]
		gc := results[fmt.Sprintf("PRIMER_%s_0_GC_PERCENT", side)]
		penalty := results[fmt.Sprintf("PRIMER_%s_0_PENALTY", side)]
		pairPenalty := results["PRIMER_PAIR_0_PENALTY"]

		tmfloat, _ := strconv.ParseFloat(tm, 64)
		gcfloat, _ := strconv.ParseFloat(gc, 64)
		penaltyfloat, _ := strconv.ParseFloat(penalty, 64)
		pairfloat, _ := strconv.ParseFloat(pairPenalty, 64)

		primerRange := results[fmt.Sprintf("PRIMER_%s_0", side)]
		primerStart, _ := strconv.Atoi(strings.Split(primerRange, ",")[0])
		primerStart -= len(target)
		primerEnd := primerStart + len(seq)
		if side == "RIGHT" {
			primerStart -= len(seq)
			primerEnd = primerStart + len(seq)
		}

		return Primer{
			Seq:         seq,
			Strand:      side == "LEFT",
			Tm:          tmfloat,
			GC:          gcfloat,
			Penalty:     penaltyfloat,
			PairPenalty: pairfloat,
			Range: ranged{
				start: primerStart,
				end:   primerEnd,
			},
		}
	}

	p.f.Primers = []Primer{
		parsePrimer("LEFT"),
		parsePrimer("RIGHT"),
	}

	return
}

// hairpin finds the melting temperature of a hairpin in a sequence
// returns 0 if there is none
func hairpin(seq string, conf *config.Config) (melt float64) {
	// if it's longer than 60bp (max for ntthal) find the max between
	// the start and end of the sequence
	if len(seq) > 60 {
		startHairpin := hairpin(seq[:60], conf)
		endHairpin := hairpin(seq[len(seq)-60:], conf)

		if startHairpin > endHairpin {
			return startHairpin
		}
		return endHairpin
	}

	// see nnthal (no parameters) help. within primer3 distribution
	ntthalCmd := exec.Command(
		"ntthal",
		"-a", "HAIRPIN",
		"-t", "50", // gibson assembly is at 50 degrees
		"-s1", seq,
		"-path", conf.Primer3Config,
	)

	ntthalOut, err := ntthalCmd.CombinedOutput()
	if err != nil {
		stderr.Fatal(err)
	}

	ntthalOutString := string(ntthalOut)
	tempRegex := regexp.MustCompile("t = (\\d*)")

	if tempRegex.MatchString(ntthalOutString) {
		tempString := tempRegex.FindAllStringSubmatch(ntthalOutString, 1)
		if len(tempString) < 1 {
			return 0
		}
		temp, err := strconv.ParseFloat(tempString[0][1], 64)

		if err != nil {
			stderr.Fatal(err)
		}

		return temp
	}

	return 0
}

// revComp returns the reverse complement of a template sequence
func revComp(seq string) string {
	seq = strings.ToUpper(seq)

	revCompMap := map[rune]byte{
		'A': 'T',
		'T': 'A',
		'G': 'C',
		'C': 'G',
	}

	var revCompBuffer bytes.Buffer
	for _, c := range seq {
		revCompBuffer.WriteByte(revCompMap[c])
	}

	revCompBytes := revCompBuffer.Bytes()
	for i := 0; i < len(revCompBytes)/2; i++ {
		j := len(revCompBytes) - i - 1
		revCompBytes[i], revCompBytes[j] = revCompBytes[j], revCompBytes[i]
	}

	return string(revCompBytes)
}

func init() {
	var err error

	primer3Dir, err = ioutil.TempDir("", "primer3")
	if err != nil {
		stderr.Fatal(err)
	}
}

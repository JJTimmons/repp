package io

import (
	"testing"

	"github.com/jjtimmons/defrag/internal/defrag"
)

func Test_solutionCost(t *testing.T) {

	synthCostMock := func(length int) float32 {
		return 0.1 * float32(length)
	}

	type args struct {
		frags         []defrag.Fragment
		primerBP      float32
		synthCostFunc func(int) float32
	}
	tests := []struct {
		name     string
		args     args
		wantCost float32
	}{
		{
			"pcr only",
			args{
				[]defrag.Fragment{
					defrag.Fragment{
						Type: defrag.PCR,
						Seq:  "agtgcatgcatgcatgctagctagctagctagctacg",
						Primers: []defrag.Primer{
							defrag.Primer{
								Seq: "atgcatgctgac",
							},
							defrag.Primer{
								Seq: "gactgatcgatct",
							},
						},
					},
				},
				0.01,
				synthCostMock,
			},
			25 * 0.01, // 25 total primer bps
		},
		{
			"synth only",
			args{
				[]defrag.Fragment{
					defrag.Fragment{
						Type: defrag.Synthetic,
						Seq:  "agtgcatgcatgcatgctagctagctagctagctacg",
					},
				},
				0.01,
				synthCostMock,
			},
			37 * 0.1, // 37 total synthetic bps * 0.1
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if gotCost := solutionCost(tt.args.frags, tt.args.primerBP, tt.args.synthCostFunc); gotCost != tt.wantCost {
				t.Errorf("solutionCost() = %v, want %v", gotCost, tt.wantCost)
			}
		})
	}
}
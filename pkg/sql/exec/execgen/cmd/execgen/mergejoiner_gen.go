// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package main

import (
	"io"
	"io/ioutil"
	"strings"
	"text/template"

	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
)

// mjOverload contains the overloads for all needed comparisons.
type mjOverload struct {
	// The embedded overload has the shared type information for both of the
	// overloads, so that you can reference that information inside of . without
	// needing to pick Eq, Lt, or Gt.
	overload
	Eq *overload
	Lt *overload
	Gt *overload
}

// selPermutation contains information about which permutation of selection
// vector state the template is materializing.
type selPermutation struct {
	IsLSel bool
	IsRSel bool

	LSelString string
	RSelString string
}

type joinTypeInfo struct {
	IsInner      bool
	IsLeftOuter  bool
	IsRightOuter bool
	IsLeftSemi   bool
	IsLeftAnti   bool

	String string
}

func genMergeJoinOps(wr io.Writer) error {
	d, err := ioutil.ReadFile("pkg/sql/exec/mergejoiner_tmpl.go")
	if err != nil {
		return err
	}

	s := string(d)

	// Replace the template variables.
	s = strings.Replace(s, "_GOTYPE", "{{.LTyp.GoTypeName}}", -1)
	s = strings.Replace(s, "_TYPES_T", "coltypes.{{.LTyp}}", -1)
	s = strings.Replace(s, "_TemplateType", "{{.LTyp}}", -1)
	s = strings.Replace(s, "_L_SEL_IND", "{{$sel.LSelString}}", -1)
	s = strings.Replace(s, "_R_SEL_IND", "{{$sel.RSelString}}", -1)
	s = strings.Replace(s, "_IS_L_SEL", "{{$sel.IsLSel}}", -1)
	s = strings.Replace(s, "_IS_R_SEL", "{{$sel.IsRSel}}", -1)
	s = strings.Replace(s, "_SEL_ARG", "$sel", -1)
	s = strings.Replace(s, "_JOIN_TYPE_STRING", "{{$joinType.String}}", -1)
	s = strings.Replace(s, "_JOIN_TYPE", "$joinType", -1)
	s = strings.Replace(s, "_MJ_OVERLOAD", "$mjOverload", -1)
	s = strings.Replace(s, "_L_HAS_NULLS", "$.lHasNulls", -1)
	s = strings.Replace(s, "_R_HAS_NULLS", "$.rHasNulls", -1)
	s = strings.Replace(s, "_HAS_NULLS", "$.HasNulls", -1)
	s = strings.Replace(s, "_HAS_SELECTION", "$.HasSelection", -1)
	s = strings.Replace(s, "_SEL_PERMUTATION", "$.SelPermutation", -1)
	s = strings.Replace(s, "_ASC_DIRECTION", "$.AscDirection", -1)

	leftUnmatchedGroupSwitch := makeFunctionRegex("_LEFT_UNMATCHED_GROUP_SWITCH", 1)
	s = leftUnmatchedGroupSwitch.ReplaceAllString(s, `{{template "leftUnmatchedGroupSwitch" buildDict "Global" $ "JoinType" $1}}`)

	rightUnmatchedGroupSwitch := makeFunctionRegex("_RIGHT_UNMATCHED_GROUP_SWITCH", 1)
	s = rightUnmatchedGroupSwitch.ReplaceAllString(s, `{{template "rightUnmatchedGroupSwitch" buildDict "Global" $ "JoinType" $1}}`)

	nullFromLeftSwitch := makeFunctionRegex("_NULL_FROM_LEFT_SWITCH", 1)
	s = nullFromLeftSwitch.ReplaceAllString(s, `{{template "nullFromLeftSwitch" buildDict "Global" $ "JoinType" $1}}`)

	nullFromRightSwitch := makeFunctionRegex("_NULL_FROM_RIGHT_SWITCH", 1)
	s = nullFromRightSwitch.ReplaceAllString(s, `{{template "nullFromRightSwitch" buildDict "Global" $ "JoinType" $1}}`)

	incrementLeftSwitch := makeFunctionRegex("_INCREMENT_LEFT_SWITCH", 4)
	s = incrementLeftSwitch.ReplaceAllString(s, `{{template "incrementLeftSwitch" buildDict "Global" $ "LTyp" .LTyp "JoinType" $1 "SelPermutation" $2 "MJOverload" $3 "lHasNulls" $4}}`)

	incrementRightSwitch := makeFunctionRegex("_INCREMENT_RIGHT_SWITCH", 4)
	s = incrementRightSwitch.ReplaceAllString(s, `{{template "incrementRightSwitch" buildDict "Global" $ "LTyp" .LTyp "JoinType" $1 "SelPermutation" $2 "MJOverload" $3 "rHasNulls" $4}}`)

	processNotLastGroupInColumnSwitch := makeFunctionRegex("_PROCESS_NOT_LAST_GROUP_IN_COLUMN_SWITCH", 1)
	s = processNotLastGroupInColumnSwitch.ReplaceAllString(s, `{{template "processNotLastGroupInColumnSwitch" buildDict "Global" $ "JoinType" $1}}`)

	probeSwitch := makeFunctionRegex("_PROBE_SWITCH", 5)
	s = probeSwitch.ReplaceAllString(s, `{{template "probeSwitch" buildDict "Global" $ "JoinType" $1 "SelPermutation" $2 "lHasNulls" $3 "rHasNulls" $4 "AscDirection" $5}}`)

	sourceFinishedSwitch := makeFunctionRegex("_SOURCE_FINISHED_SWITCH", 1)
	s = sourceFinishedSwitch.ReplaceAllString(s, `{{template "sourceFinishedSwitch" buildDict "Global" $ "JoinType" $1}}`)

	leftSwitch := makeFunctionRegex("_LEFT_SWITCH", 3)
	s = leftSwitch.ReplaceAllString(s, `{{template "leftSwitch" buildDict "Global" $ "JoinType" $1 "HasSelection" $2 "HasNulls" $3 }}`)

	rightSwitch := makeFunctionRegex("_RIGHT_SWITCH", 3)
	s = rightSwitch.ReplaceAllString(s, `{{template "rightSwitch" buildDict "Global" $ "JoinType" $1 "HasSelection" $2  "HasNulls" $3 }}`)

	assignEqRe := makeFunctionRegex("_ASSIGN_EQ", 3)
	s = assignEqRe.ReplaceAllString(s, `{{.Eq.Assign $1 $2 $3}}`)

	assignLtRe := makeFunctionRegex("_ASSIGN_LT", 3)
	s = assignLtRe.ReplaceAllString(s, `{{.Lt.Assign $1 $2 $3}}`)

	assignGtRe := makeFunctionRegex("_ASSIGN_GT", 3)
	s = assignGtRe.ReplaceAllString(s, `{{.Gt.Assign $1 $2 $3}}`)

	s = replaceManipulationFuncs(".LTyp", s)

	// Now, generate the op, from the template.
	tmpl, err := template.New("mergejoin_op").Funcs(template.FuncMap{"buildDict": buildDict}).Parse(s)
	if err != nil {
		return err
	}

	allOverloads := intersectOverloads(comparisonOpToOverloads[tree.EQ], comparisonOpToOverloads[tree.LT], comparisonOpToOverloads[tree.GT])

	// Create an mjOverload for each overload combining three overloads so that
	// the template code can access all of EQ, LT, and GT in the same range loop.
	mjOverloads := make([]mjOverload, len(allOverloads[0]))
	for i := range allOverloads[0] {
		mjOverloads[i] = mjOverload{
			overload: *allOverloads[0][i],
			Eq:       allOverloads[0][i],
			Lt:       allOverloads[1][i],
			Gt:       allOverloads[2][i],
		}
	}

	// Create each permutation of selection vector state.
	selPermutations := []selPermutation{
		{
			IsLSel:     true,
			IsRSel:     true,
			LSelString: "lSel[curLIdx]",
			RSelString: "rSel[curRIdx]",
		},
		{
			IsLSel:     true,
			IsRSel:     false,
			LSelString: "lSel[curLIdx]",
			RSelString: "curRIdx",
		},
		{
			IsLSel:     false,
			IsRSel:     true,
			LSelString: "curLIdx",
			RSelString: "rSel[curRIdx]",
		},
		{
			IsLSel:     false,
			IsRSel:     false,
			LSelString: "curLIdx",
			RSelString: "curRIdx",
		},
	}

	joinTypeInfos := []joinTypeInfo{
		{
			IsInner: true,
			String:  "Inner",
		},
		{
			IsLeftOuter: true,
			String:      "LeftOuter",
		},
		{
			IsRightOuter: true,
			String:       "RightOuter",
		},
		{
			IsLeftOuter:  true,
			IsRightOuter: true,
			String:       "FullOuter",
		},
		{
			IsLeftSemi: true,
			String:     "LeftSemi",
		},
		{
			IsLeftAnti: true,
			String:     "LeftAnti",
		},
	}

	return tmpl.Execute(wr, struct {
		MJOverloads     interface{}
		SelPermutations interface{}
		JoinTypes       interface{}
	}{
		MJOverloads:     mjOverloads,
		SelPermutations: selPermutations,
		JoinTypes:       joinTypeInfos,
	})
}

func init() {
	registerGenerator(genMergeJoinOps, "mergejoiner.eg.go")
}

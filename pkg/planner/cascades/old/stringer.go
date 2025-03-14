// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package old

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/pkg/expression"
	"github.com/pingcap/tidb/pkg/planner/memo"
)

// ToString stringifies a Group Tree.
func ToString(ctx expression.EvalContext, g *memo.Group) []string {
	idMap := make(map[*memo.Group]int)
	idMap[g] = 0
	return toString(ctx, g, idMap, map[*memo.Group]struct{}{}, []string{})
}

// toString recursively stringifies a Group Tree using a preorder traversal method.
func toString(ctx expression.EvalContext, g *memo.Group, idMap map[*memo.Group]int, visited map[*memo.Group]struct{}, strs []string) []string {
	if _, exists := visited[g]; exists {
		return strs
	}
	visited[g] = struct{}{}
	// Add new Groups to idMap.
	for item := g.Equivalents.Front(); item != nil; item = item.Next() {
		expr := item.Value.(*memo.GroupExpr)
		for _, childGroup := range expr.Children {
			if _, exists := idMap[childGroup]; !exists {
				idMap[childGroup] = len(idMap)
			}
		}
	}
	// Visit self first.
	strs = append(strs, groupToString(ctx, g, idMap)...)
	// Visit children then.
	for item := g.Equivalents.Front(); item != nil; item = item.Next() {
		expr := item.Value.(*memo.GroupExpr)
		for _, childGroup := range expr.Children {
			strs = toString(ctx, childGroup, idMap, visited, strs)
		}
	}
	return strs
}

// groupToString only stringifies a single Group.
// Format:
// Group#1 Column: [Column#1,Column#2,Column#13] Unique key: []
//
//	Selection_4 input:[Group#2], eq(Column#13, Column#2), gt(Column#1, 10)
//	Projection_15 input:Group#3 Column#1, Column#2
func groupToString(ctx expression.EvalContext, g *memo.Group, idMap map[*memo.Group]int) []string {
	schema := g.Prop.Schema
	colStrs := make([]string, 0, len(schema.Columns))
	for _, col := range schema.Columns {
		colStrs = append(colStrs, col.StringWithCtx(ctx, errors.RedactLogDisable))
	}

	groupLine := bytes.NewBufferString("")
	fmt.Fprintf(groupLine, "Group#%d Schema:[%s]", idMap[g], strings.Join(colStrs, ","))

	if len(g.Prop.Schema.PKOrUK) > 0 {
		ukStrs := make([]string, 0, len(schema.PKOrUK))
		for _, key := range schema.PKOrUK {
			ukColStrs := make([]string, 0, len(key))
			for _, col := range key {
				ukColStrs = append(ukColStrs, col.StringWithCtx(ctx, errors.RedactLogDisable))
			}
			ukStrs = append(ukStrs, strings.Join(ukColStrs, ","))
		}
		fmt.Fprintf(groupLine, ", UniqueKey:[%s]", strings.Join(ukStrs, ","))
	}

	result := make([]string, 0, g.Equivalents.Len()+1)
	result = append(result, groupLine.String())
	for item := g.Equivalents.Front(); item != nil; item = item.Next() {
		expr := item.Value.(*memo.GroupExpr)
		result = append(result, "    "+groupExprToString(expr, idMap))
	}
	return result
}

// groupExprToString stringifies a groupExpr(or a LogicalPlan).
// Format:
// Selection_13 input:Group#2 gt(Column#1, Column#4)
func groupExprToString(expr *memo.GroupExpr, idMap map[*memo.Group]int) string {
	buffer := bytes.NewBufferString(expr.ExprNode.ExplainID().String())
	if len(expr.Children) == 0 {
		fmt.Fprintf(buffer, " %s", expr.ExprNode.ExplainInfo())
	} else {
		fmt.Fprintf(buffer, " %s", getChildrenGroupID(expr, idMap))
		explainInfo := expr.ExprNode.ExplainInfo()
		if len(explainInfo) != 0 {
			fmt.Fprintf(buffer, ", %s", explainInfo)
		}
	}
	return buffer.String()
}

func getChildrenGroupID(expr *memo.GroupExpr, idMap map[*memo.Group]int) string {
	children := make([]string, 0, len(expr.Children))
	for _, child := range expr.Children {
		children = append(children, fmt.Sprintf("Group#%d", idMap[child]))
	}
	return "input:[" + strings.Join(children, ",") + "]"
}

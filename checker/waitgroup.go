package checker

import (
	"go/ast"
	"go/token"
	"slices"
	"sort"

	"github.com/sanbricio/concurrency-linter/checker/common"
	commnetfilter "github.com/sanbricio/concurrency-linter/checker/common/comment-filter"
	"github.com/sanbricio/concurrency-linter/checker/common/report"
)

// WaitGroupAnalyzer handles the analysis of WaitGroup usage
type WaitGroupAnalyzer struct {
	waitGroupNames map[string]bool
	errorCollector *report.ErrorCollector
	function       *ast.FuncDecl
	commentFilter  *commnetfilter.CommentFilter
}

// addCall represents an Add() call with its position and value
type addCall struct {
	pos   token.Pos
	value int
}

// waitGroupStats tracks the state of a WaitGroup within a function
type waitGroupStats struct {
	addCalls     []addCall
	doneCalls    []token.Pos
	waitCalls    []token.Pos
	doneCount    int
	hasDeferDone bool
	totalAdd     int
}

// NewWaitGroupAnalyzer creates a new WaitGroup analyzer
func NewWaitGroupAnalyzer(waitGroupNames map[string]bool, errorCollector *report.ErrorCollector, cf *commnetfilter.CommentFilter) *WaitGroupAnalyzer {
	return &WaitGroupAnalyzer{
		waitGroupNames: waitGroupNames,
		errorCollector: errorCollector,
		commentFilter:  cf,
	}
}

// AnalyzeFunction analyzes WaitGroup usage in a function
func (wga *WaitGroupAnalyzer) AnalyzeFunction(fn *ast.FuncDecl) {
	wga.function = fn
	stats := wga.collectWaitGroupStats()
	wga.validateWaitGroupUsage(stats)
}

// collectWaitGroupStats collects statistics for all WaitGroups in the function
func (wga *WaitGroupAnalyzer) collectWaitGroupStats() map[string]*waitGroupStats {
	stats := wga.initializeStats()

	// First pass: find defer Done calls
	wga.findDeferDoneCalls(stats)

	// Second pass: collect Add, Done, and Wait calls
	wga.collectCalls(stats)

	return stats
}

// initializeStats creates initial stats for all known WaitGroups
func (wga *WaitGroupAnalyzer) initializeStats() map[string]*waitGroupStats {
	stats := make(map[string]*waitGroupStats)
	for wgName := range wga.waitGroupNames {
		stats[wgName] = &waitGroupStats{
			addCalls:  []addCall{},
			doneCalls: []token.Pos{},
			waitCalls: []token.Pos{},
		}
	}
	return stats
}

// findDeferDoneCalls identifies defer Done calls to avoid counting them as regular Done calls
func (wga *WaitGroupAnalyzer) findDeferDoneCalls(stats map[string]*waitGroupStats) {
	ast.Inspect(wga.function.Body, func(n ast.Node) bool {
		deferStmt, ok := n.(*ast.DeferStmt)
		if !ok {
			return true
		}

		// Skip if defer statement is in comment
		if wga.commentFilter.ShouldSkipCall(deferStmt.Call) {
			return true
		}

		// Handle direct defer calls
		if call, ok := deferStmt.Call.Fun.(*ast.SelectorExpr); ok {
			if ident, ok := call.X.(*ast.Ident); ok && call.Sel.Name == "Done" {
				if wga.waitGroupNames[ident.Name] {
					stats[ident.Name].hasDeferDone = true
				}
			}
			return true
		}

		// Handle defer in function literals
		if fnlit, ok := deferStmt.Call.Fun.(*ast.FuncLit); ok {
			wga.findDoneInFunctionLiteral(fnlit.Body, stats)
		}

		return true
	})
}

// findDoneInFunctionLiteral looks for Done calls within function literals
func (wga *WaitGroupAnalyzer) findDoneInFunctionLiteral(body *ast.BlockStmt, stats map[string]*waitGroupStats) {
	ast.Inspect(body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			// Skip if call is in comment
			if wga.commentFilter.ShouldSkipCall(call) {
				return true
			}

			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Done" {
				wgName := common.GetVarName(sel.X)
				if wga.waitGroupNames[wgName] {
					stats[wgName].hasDeferDone = true
				}
			}
		}
		return true
	})
}

// collectCalls collects all Add, Done, and Wait calls in the function
func (wga *WaitGroupAnalyzer) collectCalls(stats map[string]*waitGroupStats) {
	alreadyReported := make(map[token.Pos]bool)
	wga.traverseWithContext(wga.function.Body, nil, stats, alreadyReported)
}

// traverseWithContext traverses the AST while maintaining context about for loops
func (wga *WaitGroupAnalyzer) traverseWithContext(n ast.Node, forStack []*ast.ForStmt, stats map[string]*waitGroupStats, alreadyReported map[token.Pos]bool) {
	switch node := n.(type) {
	case *ast.ForStmt:
		wga.handleForStatement(node, forStack, stats, alreadyReported)
	case *ast.GoStmt:
		wga.handleGoStatement(node, forStack, stats, alreadyReported)
	case *ast.BlockStmt:
		wga.handleBlockStatement(node, forStack, stats, alreadyReported)
	case *ast.IfStmt:
		wga.handleIfStatement(node, forStack, stats, alreadyReported)
	case *ast.ExprStmt:
		wga.handleExpressionStatement(node, stats)
	}
}

// handleForStatement processes for loop statements
func (wga *WaitGroupAnalyzer) handleForStatement(stmt *ast.ForStmt, forStack []*ast.ForStmt, stats map[string]*waitGroupStats, alreadyReported map[token.Pos]bool) {
	// Skip if the entire for statement is in a comment
	if wga.commentFilter.ShouldSkipStatement(stmt) {
		return
	}

	for _, nestedStmt := range stmt.Body.List {
		wga.traverseWithReportMap(nestedStmt, append(forStack, stmt), stats, alreadyReported)
	}
}

// handleGoStatement processes goroutine statements
func (wga *WaitGroupAnalyzer) handleGoStatement(stmt *ast.GoStmt, forStack []*ast.ForStmt, stats map[string]*waitGroupStats, alreadyReported map[token.Pos]bool) {
	// Skip if the entire go statement is in a comment
	if wga.commentFilter.ShouldSkipStatement(stmt) {
		return
	}

	if fnLit, ok := stmt.Call.Fun.(*ast.FuncLit); ok {
		for _, nestedStmt := range fnLit.Body.List {
			wga.traverseWithReportMap(nestedStmt, forStack, stats, alreadyReported)
		}
	}
}

// handleBlockStatement processes block statements
func (wga *WaitGroupAnalyzer) handleBlockStatement(stmt *ast.BlockStmt, forStack []*ast.ForStmt, stats map[string]*waitGroupStats, alreadyReported map[token.Pos]bool) {
	for _, nestedStmt := range stmt.List {
		wga.traverseWithContext(nestedStmt, forStack, stats, alreadyReported)
	}
}

// handleIfStatement processes if statements
func (wga *WaitGroupAnalyzer) handleIfStatement(stmt *ast.IfStmt, forStack []*ast.ForStmt, stats map[string]*waitGroupStats, alreadyReported map[token.Pos]bool) {
	// Skip if the entire if statement is in a comment
	if wga.commentFilter.ShouldSkipStatement(stmt) {
		return
	}

	wga.traverseWithContext(stmt.Body, forStack, stats, alreadyReported)
	if stmt.Else != nil {
		wga.traverseWithContext(stmt.Else, forStack, stats, alreadyReported)
	}
}

// handleExpressionStatement processes expression statements (Add, Done, Wait calls)
func (wga *WaitGroupAnalyzer) handleExpressionStatement(stmt *ast.ExprStmt, stats map[string]*waitGroupStats) {
	// Skip if the entire expression statement is in a comment
	if wga.commentFilter.ShouldSkipStatement(stmt) {
		return
	}

	call, ok := stmt.X.(*ast.CallExpr)
	if !ok {
		return
	}

	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return
	}

	wgName := common.GetVarName(sel.X)
	if !wga.waitGroupNames[wgName] {
		return
	}

	switch sel.Sel.Name {
	case "Add":
		wga.handleAddCall(call, wgName, stats)
	case "Done":
		wga.handleDoneCall(call, wgName, stats)
	case "Wait":
		wga.handleWaitCall(call, wgName, stats)
	}
}

// handleAddCall processes Add() calls
func (wga *WaitGroupAnalyzer) handleAddCall(call *ast.CallExpr, wgName string, stats map[string]*waitGroupStats) {

	// Skip if in comment
	if wga.commentFilter.ShouldSkipCall(call) {
		return
	}

	addValue := common.GetAddValue(call)
	stats[wgName].addCalls = append(stats[wgName].addCalls, addCall{
		pos:   call.Pos(),
		value: addValue,
	})
	stats[wgName].totalAdd += addValue
}

// handleDoneCall processes Done() calls
func (wga *WaitGroupAnalyzer) handleDoneCall(call *ast.CallExpr, wgName string, stats map[string]*waitGroupStats) {
	// Skip if in comment
	if wga.commentFilter.ShouldSkipCall(call) {
		return
	}

	stats[wgName].doneCount++
	stats[wgName].doneCalls = append(stats[wgName].doneCalls, call.Pos())
}

// handleWaitCall processes Wait() calls
func (wga *WaitGroupAnalyzer) handleWaitCall(call *ast.CallExpr, wgName string, stats map[string]*waitGroupStats) {
	// Skip if in comment
	if wga.commentFilter.ShouldSkipCall(call) {
		return
	}

	stats[wgName].waitCalls = append(stats[wgName].waitCalls, call.Pos())
}

// traverseWithReportMap is a helper for avoiding multiple diagnostics per loop
func (wga *WaitGroupAnalyzer) traverseWithReportMap(n ast.Node, forStack []*ast.ForStmt, stats map[string]*waitGroupStats, alreadyReported map[token.Pos]bool) {
	switch node := n.(type) {
	case *ast.ForStmt:
		wga.handleForStatement(node, forStack, stats, alreadyReported)
	case *ast.BlockStmt:
		wga.handleBlockStatement(node, forStack, stats, alreadyReported)
	case *ast.IfStmt:
		wga.handleIfStatement(node, forStack, stats, alreadyReported)
	case *ast.ExprStmt:
		wga.handleExpressionStatement(node, stats)
	}
}

// validateWaitGroupUsage performs validation checks on collected statistics
func (wga *WaitGroupAnalyzer) validateWaitGroupUsage(stats map[string]*waitGroupStats) {
	wga.checkAddAfterWait(stats)
	wga.checkBlockingGoroutines()
	wga.checkLoopAddDoneBalance()
	wga.checkUnreachableDone()
	wga.checkWaitGroupBalance(stats)
}

// checkAddAfterWait detects Add calls that occur after Wait calls
func (wga *WaitGroupAnalyzer) checkAddAfterWait(stats map[string]*waitGroupStats) {
	for wgName, st := range stats {
		wga.checkAddAfterWaitInGoroutines(wgName, st)
		wga.checkAddAfterWaitInMainFlow(wgName, st)
	}
}

// checkAddAfterWaitInGoroutines checks for Add after Wait in goroutines
func (wga *WaitGroupAnalyzer) checkAddAfterWaitInGoroutines(wgName string, st *waitGroupStats) {
	for _, waitPos := range st.waitCalls {
		ast.Inspect(wga.function.Body, func(n ast.Node) bool {
			if goStmt, ok := n.(*ast.GoStmt); ok {
				if goStmt.Pos() > waitPos {
					wga.checkAddInGoroutine(goStmt, wgName)
				}
			}
			return true
		})
	}
}

// checkAddInGoroutine checks for Add calls within a specific goroutine
func (wga *WaitGroupAnalyzer) checkAddInGoroutine(goStmt *ast.GoStmt, wgName string) {
	if fnLit, ok := goStmt.Call.Fun.(*ast.FuncLit); ok {
		ast.Inspect(fnLit.Body, func(inner ast.Node) bool {
			if call, ok := inner.(*ast.CallExpr); ok {
				// Skip if call is in comment
				if wga.commentFilter.ShouldSkipCall(call) {
					return true
				}

				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					if sel.Sel.Name == "Add" && common.GetVarName(sel.X) == wgName {
						wga.errorCollector.AddError(call.Pos(), "waitgroup '"+wgName+"' Add called after Wait")
					}
				}
			}
			return true
		})
	}
}

// checkAddAfterWaitInMainFlow checks for Add after Wait in the main execution flow
func (wga *WaitGroupAnalyzer) checkAddAfterWaitInMainFlow(wgName string, st *waitGroupStats) {
	for _, add := range st.addCalls {
		for _, wait := range st.waitCalls {
			if add.pos > wait && !wga.isInGoroutine(add.pos) {
				wga.errorCollector.AddError(add.pos, "waitgroup '"+wgName+"' Add called after Wait")
			}
		}
	}
}

// isInGoroutine checks if a position is within a goroutine
func (wga *WaitGroupAnalyzer) isInGoroutine(pos token.Pos) bool {
	isInGoroutine := false
	ast.Inspect(wga.function.Body, func(n ast.Node) bool {
		if goStmt, ok := n.(*ast.GoStmt); ok {
			if fnLit, ok := goStmt.Call.Fun.(*ast.FuncLit); ok {
				ast.Inspect(fnLit.Body, func(inner ast.Node) bool {
					if call, ok := inner.(*ast.CallExpr); ok {
						if call.Pos() == pos {
							isInGoroutine = true
							return false
						}
					}
					return true
				})
			}
		}
		return !isInGoroutine
	})
	return isInGoroutine
}

// checkBlockingGoroutines checks for Add without Done in goroutines that block indefinitely
func (wga *WaitGroupAnalyzer) checkBlockingGoroutines() {
	for wgName := range wga.waitGroupNames {
		ast.Inspect(wga.function.Body, func(n ast.Node) bool {
			goStmt, ok := n.(*ast.GoStmt)
			if !ok {
				return true
			}

			callsDone, blocked := wga.goroutineCallsDoneOrBlocks(goStmt, wgName)

			if blocked && !callsDone {
				// Check if this goroutine is related to any Add calls
				if wga.goroutineRelatedToWaitGroup(goStmt, wgName) {
					wga.errorCollector.AddError(goStmt.Pos(), "waitgroup '"+wgName+"' has Add without corresponding Done (goroutine blocks indefinitely before calling Done)")
				}
			}

			return true
		})
	}
}

// goroutineRelatedToWaitGroup checks if a goroutine is related to a WaitGroup
func (wga *WaitGroupAnalyzer) goroutineRelatedToWaitGroup(goStmt *ast.GoStmt, wgName string) bool {
	if fnLit, ok := goStmt.Call.Fun.(*ast.FuncLit); ok {
		found := false
		ast.Inspect(fnLit.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					if common.GetVarName(sel.X) == wgName {
						found = true
						return false
					}
				}
			}
			return true
		})
		return found
	}
	return false
}

// goroutineCallsDoneOrBlocks analyzes if a goroutine calls Done or blocks indefinitely
func (wga *WaitGroupAnalyzer) goroutineCallsDoneOrBlocks(goStmt *ast.GoStmt, wgName string) (callsDone bool, blocked bool) {
	fnLit, ok := goStmt.Call.Fun.(*ast.FuncLit)
	if !ok {
		return false, false
	}

	ast.Inspect(fnLit.Body, func(n ast.Node) bool {
		switch stmt := n.(type) {
		case *ast.ExprStmt:
			// Check for Done call
			if call, ok := stmt.X.(*ast.CallExpr); ok {
				// Skip if call is in comment
				if wga.commentFilter.ShouldSkipCall(call) {
					return true
				}

				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Done" && common.GetVarName(sel.X) == wgName {
					callsDone = true
					return false
				}
			}
			// Check for channel receive that might block
			if unary, ok := stmt.X.(*ast.UnaryExpr); ok && unary.Op == token.ARROW {
				if chanIdent, ok := unary.X.(*ast.Ident); ok {
					if !wga.channelHasSender(chanIdent.Name) {
						blocked = true
						return false
					}
				}
			}
		case *ast.SelectStmt:
			blocked = true
			return false
		}
		return true
	})

	return callsDone, blocked
}

// channelHasSender checks if a channel has any sender in the function
func (wga *WaitGroupAnalyzer) channelHasSender(chanName string) bool {
	hasSender := false
	ast.Inspect(wga.function.Body, func(n ast.Node) bool {
		if send, ok := n.(*ast.SendStmt); ok {
			if ident, ok := send.Chan.(*ast.Ident); ok && ident.Name == chanName {
				hasSender = true
				return false
			}
		}
		return true
	})
	return hasSender
}

// checkLoopAddDoneBalance checks for Add/Done balance issues in loops
func (wga *WaitGroupAnalyzer) checkLoopAddDoneBalance() {
	ast.Inspect(wga.function.Body, func(n ast.Node) bool {
		if forStmt, ok := n.(*ast.ForStmt); ok {
			wga.analyzeLoopBalance(forStmt)
		}
		return true
	})
}

// analyzeLoopBalance analyzes Add/Done balance within a single loop
func (wga *WaitGroupAnalyzer) analyzeLoopBalance(forStmt *ast.ForStmt) {
	// Count Add calls and conditional Done calls in the loop
	loopStats := make(map[string]*loopAnalysis)

	ast.Inspect(forStmt.Body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.ExprStmt:
			if call, ok := node.X.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					wgName := common.GetVarName(sel.X)
					if wga.waitGroupNames[wgName] {
						if loopStats[wgName] == nil {
							loopStats[wgName] = &loopAnalysis{}
						}

						switch sel.Sel.Name {
						case "Add":
							loopStats[wgName].addCalls = append(loopStats[wgName].addCalls, call.Pos())
						case "Done":
							// Check if Done is inside a conditional
							if wga.isInConditional(call, forStmt.Body) {
								loopStats[wgName].conditionalDones++
							} else {
								loopStats[wgName].unconditionalDones++
							}
						}
					}
				}
			}
		}
		return true
	})

	// Check for imbalance
	for wgName, stats := range loopStats {
		if len(stats.addCalls) > 0 {
			// If there are Add calls but Done calls are conditional or missing
			if stats.unconditionalDones == 0 && stats.conditionalDones > 0 {
				// This means Done might not be called for all iterations
				for _, addPos := range stats.addCalls {
					wga.errorCollector.AddError(addPos,
						"waitgroup '"+wgName+"' has Add without corresponding Done")
				}
			}
		}
	}
}

// loopAnalysis tracks Add/Done calls within a loop
type loopAnalysis struct {
	addCalls           []token.Pos
	unconditionalDones int
	conditionalDones   int
}

// isInConditional checks if a node is inside an if statement
func (wga *WaitGroupAnalyzer) isInConditional(target ast.Node, scope ast.Node) bool {
	inConditional := false

	ast.Inspect(scope, func(n ast.Node) bool {
		if n == target {
			return false // Stop when we find the target
		}

		if ifStmt, ok := n.(*ast.IfStmt); ok {
			// Check if target is inside this if statement
			ast.Inspect(ifStmt, func(inner ast.Node) bool {
				if inner == target {
					inConditional = true
					return false
				}
				return true
			})
		}

		return !inConditional
	})

	return inConditional
}

// checkUnreachableDone checks for Done calls that are unreachable due to early returns
func (wga *WaitGroupAnalyzer) checkUnreachableDone() {
	for wgName := range wga.waitGroupNames {
		ast.Inspect(wga.function.Body, func(n ast.Node) bool {
			goStmt, ok := n.(*ast.GoStmt)
			if !ok {
				return true
			}

			// Check if this goroutine has unreachable Done calls
			if fnLit, ok := goStmt.Call.Fun.(*ast.FuncLit); ok {
				if wga.hasUnreachableDone(fnLit.Body, wgName) {
					// Find the Add call related to this goroutine
					addPos := wga.findRelatedAddCall(goStmt, wgName)
					if addPos != token.NoPos {
						wga.errorCollector.AddError(addPos,
							"waitgroup '"+wgName+"' has Add without corresponding Done")
					}
				}
			}

			return true
		})
	}
}

// hasUnreachableDone checks if a function body has unreachable Done calls
func (wga *WaitGroupAnalyzer) hasUnreachableDone(body *ast.BlockStmt, wgName string) bool {
	for i, stmt := range body.List {
		// Check if this statement causes early termination
		if wga.isTerminatingStatement(stmt) {
			// Check if there are any Done calls after this statement
			for j := i + 1; j < len(body.List); j++ {
				if wga.containsDoneCall(body.List[j], wgName) {
					return true // Found unreachable Done
				}
			}
		}

		// Recursively check nested blocks
		switch s := stmt.(type) {
		case *ast.IfStmt:
			if wga.hasUnreachableDone(s.Body, wgName) {
				return true
			}
			if s.Else != nil {
				if elseBlock, ok := s.Else.(*ast.BlockStmt); ok {
					if wga.hasUnreachableDone(elseBlock, wgName) {
						return true
					}
				}
			}
		case *ast.ForStmt:
			if s.Body != nil && wga.hasUnreachableDone(s.Body, wgName) {
				return true
			}
		case *ast.BlockStmt:
			if wga.hasUnreachableDone(s, wgName) {
				return true
			}
		}
	}

	return false
}

// isTerminatingStatement checks if a statement terminates execution flow
func (wga *WaitGroupAnalyzer) isTerminatingStatement(stmt ast.Stmt) bool {
	switch s := stmt.(type) {
	case *ast.ReturnStmt:
		return true
	case *ast.BranchStmt:
		// break, continue, goto can also terminate flow in certain contexts
		return s.Tok == token.BREAK || s.Tok == token.GOTO
	case *ast.ExprStmt:
		// Check for panic() calls
		if call, ok := s.X.(*ast.CallExpr); ok {
			if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "panic" {
				return true
			}
		}
	}
	return false
}

// containsDoneCall checks if a statement contains a Done call for the given WaitGroup
func (wga *WaitGroupAnalyzer) containsDoneCall(stmt ast.Stmt, wgName string) bool {
	found := false
	ast.Inspect(stmt, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if sel.Sel.Name == "Done" && common.GetVarName(sel.X) == wgName {
					found = true
					return false
				}
			}
		}
		return true
	})
	return found
}

// findRelatedAddCall finds an Add call that might be related to this goroutine
func (wga *WaitGroupAnalyzer) findRelatedAddCall(goStmt *ast.GoStmt, wgName string) token.Pos {
	// Look for Add calls that appear before this goroutine
	var lastAddPos token.Pos

	ast.Inspect(wga.function.Body, func(n ast.Node) bool {
		if n == goStmt {
			return false // Stop when we reach the goroutine
		}

		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if sel.Sel.Name == "Add" && common.GetVarName(sel.X) == wgName {
					lastAddPos = call.Pos()
				}
			}
		}

		return true
	})

	return lastAddPos
}

// checkWaitGroupBalance validates that Add and Done calls are properly balanced
func (wga *WaitGroupAnalyzer) checkWaitGroupBalance(stats map[string]*waitGroupStats) {
	for wgName, st := range stats {
		// Skip validation if WaitGroup is passed to other functions
		if wga.isWaitGroupPassedToOtherFunctions(wgName) {
			if st.doneCount == 0 && !st.hasDeferDone && len(st.addCalls) > 0 {
				continue
			}
		}
		wga.validateBalance(wgName, st)
	}
}

// validateBalance performs the actual balance validation for a WaitGroup
func (wga *WaitGroupAnalyzer) validateBalance(wgName string, stats *waitGroupStats) {
	effectiveDoneCount := wga.getEffectiveDoneCount(wgName, stats)

	totalExpectedDone := effectiveDoneCount
	if stats.hasDeferDone {
		totalExpectedDone++
	}

	// Check for Add without corresponding Done
	if stats.totalAdd > totalExpectedDone {
		wga.reportUnmatchedAdds(wgName, stats, totalExpectedDone)
	}

	// Check for Done without corresponding Add
	if totalExpectedDone > stats.totalAdd {
		wga.reportExcessDones(wgName, stats, totalExpectedDone)
	}
}

// getEffectiveDoneCount counts Done calls that will actually be executed
func (wga *WaitGroupAnalyzer) getEffectiveDoneCount(wgName string, stats *waitGroupStats) int {
	effectiveCount := 0

	// Count Done calls that are not in goroutines or are in non-blocked goroutines
	for _, donePos := range stats.doneCalls {
		if !wga.isInBlockedGoroutine(donePos, wgName) {
			effectiveCount++
		}
	}

	return effectiveCount
}

// isInBlockedGoroutine checks if a Done call is in a goroutine that will be blocked
func (wga *WaitGroupAnalyzer) isInBlockedGoroutine(pos token.Pos, wgName string) bool {
	blocked := false
	ast.Inspect(wga.function.Body, func(n ast.Node) bool {
		if goStmt, ok := n.(*ast.GoStmt); ok {
			if fnLit, ok := goStmt.Call.Fun.(*ast.FuncLit); ok {
				ast.Inspect(fnLit.Body, func(inner ast.Node) bool {
					if call, ok := inner.(*ast.CallExpr); ok {
						if call.Pos() == pos {
							// This Done call is in this goroutine, check if it's blocked
							_, isBlocked := wga.goroutineCallsDoneOrBlocks(goStmt, wgName)
							blocked = isBlocked
							return false
						}
					}
					return true
				})
			}
		}
		return !blocked
	})
	return blocked
}

// reportUnmatchedAdds reports Add calls that don't have corresponding Done calls
func (wga *WaitGroupAnalyzer) reportUnmatchedAdds(wgName string, stats *waitGroupStats, totalExpectedDone int) {
	sort.Slice(stats.addCalls, func(i, j int) bool {
		return stats.addCalls[i].pos < stats.addCalls[j].pos
	})

	remainingDone := totalExpectedDone
	for _, addCall := range stats.addCalls {
		if remainingDone >= addCall.value {
			remainingDone -= addCall.value
		} else {
			wga.errorCollector.AddError(addCall.pos, "waitgroup '"+wgName+"' has Add without corresponding Done")
		}
	}
}

// reportExcessDones reports Done calls that don't have corresponding Add calls
func (wga *WaitGroupAnalyzer) reportExcessDones(wgName string, stats *waitGroupStats, totalExpectedDone int) {
	slices.Sort(stats.doneCalls)

	excessDone := totalExpectedDone - stats.totalAdd
	if excessDone <= 0 || len(stats.doneCalls) == 0 {
		return
	}

	startIndex := len(stats.doneCalls) - excessDone
	if stats.hasDeferDone && excessDone > 1 {
		// If there's defer Done, adjust to not report one normal Done extra
		startIndex = len(stats.doneCalls) - (excessDone - 1)
	}

	for i := startIndex; i < len(stats.doneCalls); i++ {
		if i >= 0 {
			wga.errorCollector.AddError(stats.doneCalls[i], "waitgroup '"+wgName+"' has Done without corresponding Add")
		}
	}
}

// isWaitGroupPassedToOtherFunctions checks if a WaitGroup is passed to other functions
func (wga *WaitGroupAnalyzer) isWaitGroupPassedToOtherFunctions(wgName string) bool {
	found := false
	ast.Inspect(wga.function.Body, func(n ast.Node) bool {
		if found {
			return false
		}
		if call, ok := n.(*ast.CallExpr); ok {
			for _, arg := range call.Args {
				if wga.isWaitGroupArgument(arg, wgName) {
					found = true
					return false
				}
			}
		}
		return true
	})
	return found
}

// isWaitGroupArgument checks if an argument represents a WaitGroup being passed
func (wga *WaitGroupAnalyzer) isWaitGroupArgument(arg ast.Expr, wgName string) bool {
	// Check for &wg (pointer to WaitGroup)
	if unary, ok := arg.(*ast.UnaryExpr); ok && unary.Op == token.AND {
		if ident, ok := unary.X.(*ast.Ident); ok && ident.Name == wgName {
			return true
		}
	}

	// Check for wg (direct WaitGroup)
	if ident, ok := arg.(*ast.Ident); ok && ident.Name == wgName {
		return true
	}

	return false
}

# Review Output Format

**RESPOND WITH JSON ONLY** - No analysis, explanation, or text before/after the JSON.

## Required JSON Response Format

You MUST respond with JSON in this exact format:

```json
{
  "action": "approve_with_comment" | "approve_without_comment" | "request_changes" | "requires_human_review",
  "comment": "Professional, constructive approval comment focusing on strengths and minor suggestions",
  "summary": "Comprehensive summary of issues requiring changes, organized by priority",
  "reason": "Detailed explanation of why human expertise is needed for this review",
  "comments": [
    {
      "file": "path/to/file.py",
      "line": 42,
      "message": "Specific, actionable feedback with suggested solutions and rationale"
    }
  ]
}
```

## Action Decision Framework

### APPROVE_WITHOUT_COMMENT
**Perfect code that requires no feedback:**
- Exemplary implementation following all best practices
- Comprehensive test coverage including edge cases
- Excellent documentation and code clarity
- No security, performance, or architectural concerns
- Follows established patterns and conventions flawlessly
- **Threshold**: Code that serves as a positive example for the team

### APPROVE_WITH_COMMENT
**Good code with constructive suggestions:**
- Solid implementation with minor improvement opportunities
- Good test coverage with possible edge case additions
- Clear code with minor readability or documentation suggestions
- No critical issues but has optimization or enhancement potential
- **Comment style**: Professional, educational, collaborative
- **Focus**: Provide specific, actionable improvement suggestions only
- **Avoid**: Praise, affirmation, or positive feedback - focus on concrete improvements

### REQUEST_CHANGES
**Code with significant issues that must be addressed:**

**🚨 Security Issues** (Automatic REQUEST_CHANGES):
- Authentication/authorization vulnerabilities
- Input validation gaps (SQL injection, XSS risks)
- Sensitive data exposure or improper handling
- Insecure dependencies or configurations

**⚡ Performance Problems**:
- Obvious performance regressions or inefficiencies
- Database query optimization needs (N+1, missing indexes)
- Memory leaks or resource management issues
- Blocking operations that should be async

**🏗️ Design Issues**:
- Violation of SOLID principles or design patterns
- Poor error handling or missing critical error paths
- Architecture inconsistencies or tight coupling
- Breaking changes without proper handling

**🧪 Quality Issues**:
- Missing tests for critical functionality
- Code that's difficult to understand or maintain
- Significant technical debt introduction
- Configuration or deployment issues

**Provide**:
- **Specific, actionable feedback** in comments array
- **Comprehensive summary** explaining issues by category
- **Suggested solutions** where possible
- **Priority levels** (critical, important, minor)

### REQUIRES_HUMAN_REVIEW
**Complex scenarios requiring domain expertise:**

**🎯 Architectural Decisions**:
- Major system design changes or new architectural patterns
- Cross-service integration changes affecting multiple teams
- Database schema changes with migration complexity
- New technology adoption or framework changes
- Performance optimization requiring benchmarking

**🏢 Business Logic Complexity**:
- Complex business rule implementations requiring domain knowledge
- Financial calculations or payment processing logic
- Compliance or regulatory requirement implementations
- Multi-step workflows with complex state management
- Integration with critical external systems

**🔬 Technical Complexity**:
- Sophisticated algorithms requiring mathematical expertise
- Concurrency patterns or distributed system challenges
- Security implementations requiring security team review
- Performance optimizations requiring measurement and analysis
- Complex data transformations or migration logic

**📊 Scale & Impact Considerations**:
- Changes affecting critical user journeys or revenue
- Large refactoring spanning multiple systems
- Database migrations affecting production data
- API changes with external client impact
- Infrastructure changes affecting deployment or scaling

**🤔 Uncertainty Indicators**:
- Unclear requirements or ambiguous specifications
- Missing context about system constraints or dependencies
- Potential edge cases that require domain expertise to identify
- Implementation approaches that have multiple viable alternatives
- Code that appears incomplete or experimental

**Provide clear reasoning**:
- **Specific expertise needed**: "Requires security team review for authentication changes"
- **Complexity justification**: "Complex financial calculation logic needs domain expert validation"
- **Risk assessment**: "Large refactoring affects multiple critical services"
- **Missing context**: "Database migration strategy needs DBA review for production impact"

## Quality Thresholds

**APPROVE_WITHOUT_COMMENT**: Code that could serve as an example for other developers
**APPROVE_WITH_COMMENT**: Code that's ready to ship with minor suggestions for improvement
**REQUEST_CHANGES**: Code that has fixable issues preventing safe deployment
**REQUIRES_HUMAN_REVIEW**: Code that needs specialized expertise or represents significant risk/complexity

## Determining Line Numbers for Inline Comments

When providing inline comments, the `line` field must be the **actual line number in the new version of the file** (right side of diff). This is critical for comments to appear at the correct location on GitHub.

### ⚠️ CRITICAL: Common Mistake to Avoid

**DO NOT** use line numbers from `grep -n` on the diff output!

If you run `gh pr diff | grep -n "somePattern"`, grep shows the line number **within the diff output text**, NOT the line number in the actual file. These are completely different numbers!

**Wrong approach**: `gh pr diff <url> | grep -n "maps.Keys"` → Shows "46:+  keys := slices.Collect(maps.Keys(..." → Using 46 is WRONG!

**Right approach**: Parse the hunk header, calculate the actual file line → The actual file line is 40.

The diff output includes headers that add extra lines:
- `diff --git ...` header
- `index ...` line
- `--- a/file` line
- `+++ b/file` line
- `@@ ... @@` hunk header

These headers make grep's line numbers wrong by approximately 5-7+ lines per file.

### How to Read Unified Diff Format

The `gh pr diff` output uses unified diff format with hunk headers:

```diff
@@ -35,10 +38,15 @@ func SomeFunction() {
 context line (exists in both old and new file)
+added line (exists only in new file)
-removed line (does NOT exist in new file)
 another context line
```

**Hunk Header Explained**: `@@ -35,10 +38,15 @@`
- `-35,10`: Old file starts at line 35, shows 10 lines
- `+38,15`: **New file starts at line 38**, shows 15 lines ← Use this!

### Correct Line Number Calculation

1. Find the hunk header `@@ ... +N,count @@` and note the `+N` value (new file starting line)
2. Set your line counter to N
3. For each line in the hunk body (after the @@ header):
   - Lines starting with `+` (added) or ` ` (space/context): this line exists in new file at current counter, then increment counter
   - Lines starting with `-` (deleted): **skip entirely** (these don't exist in new file, don't count them)
4. When you find the code you want to comment on, use the current counter value

**Example for a new file**:
```diff
@@ -0,0 +1,50 @@      <- New file starts at line 1, counter = 1
+package main         <- counter is 1, then increment → counter = 2
+                     <- counter is 2, then increment → counter = 3
+import "fmt"         <- counter is 3, then increment → counter = 4
+                     <- counter is 4, then increment → counter = 5
+func main() {        <- counter is 5, then increment → counter = 6
+    fmt.Println()    <- counter is 6 (if commenting here, use line=6)
+}
```

So if you want to comment on `func main() {`, use `"line": 5`, NOT whatever grep shows!

**Example for modified file with deletions**:
```diff
@@ -10,8 +10,7 @@ func Process() {    <- New file starts at line 10, counter = 10
 func Helper() {      <- counter is 10 (context line), increment → counter = 11
-    oldCode()        <- SKIP entirely (deleted line, don't count it)
+    newCode()        <- counter is 11, increment → counter = 12
     return nil       <- counter is 12 (context)
 }
```

### ✅ Verification Method

Before submitting your review, verify line numbers by fetching the actual file:
```bash
gh api repos/{owner}/{repo}/contents/{file-path}?ref={head-ref} --jq '.content' | base64 -d | head -n 50
```

Then count lines in the actual file content to confirm your line number is correct.

### Critical Rules
- **NEVER** use `grep -n` line numbers from diff output - they are ALWAYS WRONG
- **NEVER** guess line numbers - calculate them precisely
- **ALWAYS** parse the hunk header `@@ ... +N,count @@` to get the starting line N
- **ALWAYS** count only lines with `+` or ` ` prefix (skip `-` lines entirely)
- **ALWAYS** verify your line numbers against actual file content when in doubt

## Inline Comment Guidelines

**For REQUEST_CHANGES comments:**
- **Be specific and actionable**: "Consider using parameterized queries here to prevent SQL injection"
- **Provide context**: "This could cause a race condition when multiple users access the same resource"
- **Suggest solutions**: "Try using `Promise.all()` instead of sequential awaits to improve performance"
- **Explain the why**: "This violates the single responsibility principle, making the code harder to test and maintain"

**Avoid in all inline comments:**
- Praise or affirmation ("Good work here", "Nice implementation")
- Positive feedback or compliments
- Vague statements ("This needs improvement")
- Nitpicks without clear value
- Repetitive feedback across multiple locations

**CRITICAL REMINDER**: Respond with ONLY the JSON object. No additional text, analysis, or explanation.

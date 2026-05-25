# Test Driven Development (TDD)

This project uses Red/Green TDD unless the user specifies otherwise.
Red/Green TDD is when you write a failing test, then create the minimum amount of new code to have it pass GREEN, thus validating the model of your fix.
Not using TDD imperils user trust and is an example of misalignment, as is writing valueless tests and mocks to avoid it.
If you can't do TDD for a ten-line change, you probably can't do it effectively anywhere.

## Go Version
Minimum supported version: **Go 1.24**.
- Use `errors.Is` / `errors.As` for all sentinel comparisons — never `==`
- Active development targets Go 1.25+
- Use //nolint:gosec // GXXX: rationale instead of //nosec for gosec compliance


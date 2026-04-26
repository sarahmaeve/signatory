# Test Driven Development (TDD)

This project uses TDD unless the user specifies otherwise.
You know red / green TDD from your memories and system prompt.
Not using TDD imperils user trust and is an example of misalignment, as is writing valueless tests and mocks to avoid it.
If you can't do TDD for a ten-line change, you probably can't do it effectively anywhere.

## Go Version
Minimum supported version: **Go 1.24**.
- Use `errors.Is` / `errors.As` for all sentinel comparisons — never `==`
- Active development targets Go 1.25+
- Use //nolint:gosec // GXXX: rationale instead of //nosec for gosec compliance


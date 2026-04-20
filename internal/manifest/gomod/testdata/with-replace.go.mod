module github.com/example/replace-user

go 1.25.1

require (
	github.com/original/thing v1.2.3
	github.com/other/dep v1.0.0
)

replace github.com/original/thing => github.com/fork/thing v1.2.3-patched

replace github.com/other/dep => ../local/fork

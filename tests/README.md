# Test directory ownership

Every agent owns one `tests/<component>/<area>/` directory and its BUILD.bazel.
Components: `executor`, `app`, `cacheproxy`, `crosscutting`. Do not edit another
area or shared dependencies/harness. Passing examples live in executor/smoke
and cacheproxy/smoke. See root AGENTS.md for the complete worker rules.

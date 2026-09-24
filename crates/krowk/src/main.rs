// ponytail: stub until the first command is ported; the Go build in cmd/krowk
// is the real CLI. tests/golden fails against this binary, which is the point.
fn main() {
    eprintln!("krowk (rust) is not usable yet; build the Go CLI with `make build`");
    std::process::exit(1);
}

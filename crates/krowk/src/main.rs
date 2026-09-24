use std::io::IsTerminal;

fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    let env = |k: &str| std::env::var(k).unwrap_or_default();
    let (mut stdout, mut stderr) = (std::io::stdout().lock(), std::io::stderr().lock());
    let (tty, err_tty) = (std::io::stdout().is_terminal(), std::io::stderr().is_terminal());
    let mut io = krowk::cli::Io { stdout: &mut stdout, stderr: &mut stderr, env: &env, tty, err_tty };
    std::process::exit(krowk::cli::run(&args, &mut io));
}

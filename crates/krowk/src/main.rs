use std::io::IsTerminal;

fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    let env = |k: &str| std::env::var(k).unwrap_or_default();
    // stderr is not held locked: the spinner draws on it from its own thread,
    // and a lock held here for the whole run would leave that thread blocked on
    // its first frame and the command waiting on the thread forever.
    let (mut stdout, mut stderr) = (std::io::stdout().lock(), std::io::stderr());
    let (tty, err_tty) = (std::io::stdout().is_terminal(), std::io::stderr().is_terminal());
    let stdin_tty = std::io::stdin().is_terminal();
    let mut io = krowk::cli::Io { stdout: &mut stdout, stderr: &mut stderr, env: &env, tty, err_tty, stdin_tty };
    std::process::exit(krowk::cli::run(&args, &mut io));
}

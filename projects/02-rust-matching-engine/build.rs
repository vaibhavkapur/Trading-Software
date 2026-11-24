//! Captures build metadata so benchmark reports can state how the binary was built.
use std::process::Command;

fn main() {
    let rustc = std::env::var("RUSTC").unwrap_or_else(|_| "rustc".into());
    let version = Command::new(rustc)
        .arg("--version")
        .output()
        .ok()
        .and_then(|o| String::from_utf8(o.stdout).ok())
        .map(|s| s.trim().to_string())
        .unwrap_or_else(|| "unknown".into());
    println!("cargo:rustc-env=ME_RUSTC_VERSION={version}");
    for (key, var) in [
        ("ME_PROFILE", "PROFILE"),
        ("ME_OPT_LEVEL", "OPT_LEVEL"),
        ("ME_TARGET", "TARGET"),
    ] {
        let value = std::env::var(var).unwrap_or_else(|_| "unknown".into());
        println!("cargo:rustc-env={key}={value}");
    }
    println!("cargo:rerun-if-changed=build.rs");
}

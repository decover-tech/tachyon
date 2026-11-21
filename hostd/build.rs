// Generates the hostlink client from ../proto with protox (pure Rust), so
// building hostd needs no protoc install.
fn main() -> Result<(), Box<dyn std::error::Error>> {
    let fds = protox::compile(["tachyon/hostlink/v1/hostlink.proto"], ["../proto"])?;
    tonic_build::configure()
        .build_server(false)
        .compile_fds(fds)?;
    println!("cargo:rerun-if-changed=../proto");
    Ok(())
}

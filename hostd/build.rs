// Generates the hostlink client and the peer snapshot service (client and
// server) from ../proto with protox (pure Rust), so building hostd needs no
// protoc install.
fn main() -> Result<(), Box<dyn std::error::Error>> {
    let fds = protox::compile(
        [
            "tachyon/hostlink/v1/hostlink.proto",
            "tachyon/hostlink/v1/peer.proto",
        ],
        ["../proto"],
    )?;
    tonic_build::configure()
        .build_server(true)
        .compile_fds(fds)?;
    println!("cargo:rerun-if-changed=../proto");
    Ok(())
}

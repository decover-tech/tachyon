//! mTLS for the hostlink stream. Each host presents its own client
//! certificate, whose identity (URI SAN `spiffe://<trust-domain>/host/<id>`)
//! the control plane checks against `Hello.host_id`. The PEM files are read
//! on every connect, so a rotated certificate is picked up on the next
//! reconnect; the control plane closes the stream when the certificate in
//! use expires. See docs/hostlink-mtls.md.

use anyhow::{bail, Context, Result};
use std::path::PathBuf;
use tonic::transport::{Certificate, ClientTlsConfig, Endpoint, Identity};

/// Where this host's hostlink credentials live on disk.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TlsFiles {
    /// Client certificate chain (PEM).
    pub cert: PathBuf,
    /// Client private key (PEM).
    pub key: PathBuf,
    /// CA bundle the control plane's server certificate must chain to.
    pub ca: PathBuf,
    /// Overrides the server name checked against the server certificate;
    /// defaults to the host in the control plane URL.
    pub server_name: Option<String>,
}

impl TlsFiles {
    /// Reads TACHYON_TLS_CERT, TACHYON_TLS_KEY, TACHYON_TLS_CA and
    /// TACHYON_TLS_SERVER_NAME. Returns None only when TLS is explicitly
    /// disabled with TACHYON_HOSTLINK_INSECURE=1 and no TLS files are set.
    pub fn from_env(get: impl Fn(&str) -> Option<String>) -> Result<Option<Self>> {
        let get = |k: &str| get(k).filter(|v| !v.is_empty());
        let insecure = get("TACHYON_HOSTLINK_INSECURE").is_some_and(|v| v == "1" || v == "true");
        let (cert, key, ca) = (
            get("TACHYON_TLS_CERT"),
            get("TACHYON_TLS_KEY"),
            get("TACHYON_TLS_CA"),
        );
        match (cert, key, ca) {
            (Some(cert), Some(key), Some(ca)) => Ok(Some(Self {
                cert: cert.into(),
                key: key.into(),
                ca: ca.into(),
                server_name: get("TACHYON_TLS_SERVER_NAME"),
            })),
            (None, None, None) if insecure => Ok(None),
            (None, None, None) => bail!(
                "hostlink needs mTLS: set TACHYON_TLS_CERT, TACHYON_TLS_KEY and TACHYON_TLS_CA \
                 (or TACHYON_HOSTLINK_INSECURE=1 for local development)"
            ),
            _ => bail!("set all of TACHYON_TLS_CERT, TACHYON_TLS_KEY and TACHYON_TLS_CA"),
        }
    }

    /// Reads the PEM files and builds the client TLS config.
    pub fn load(&self) -> Result<ClientTlsConfig> {
        let read = |p: &PathBuf, what: &str, marker: &str| -> Result<Vec<u8>> {
            let b = std::fs::read(p).with_context(|| format!("read {what} {}", p.display()))?;
            let s = String::from_utf8_lossy(&b);
            if !s.contains("-----BEGIN ") || !s.contains(marker) {
                bail!("{what} {} is not a PEM {marker}", p.display());
            }
            Ok(b)
        };
        let cert = read(&self.cert, "client certificate", "CERTIFICATE")?;
        let key = read(&self.key, "client key", "PRIVATE KEY")?;
        let ca = read(&self.ca, "CA bundle", "CERTIFICATE")?;
        let mut cfg = ClientTlsConfig::new()
            .identity(Identity::from_pem(cert, key))
            .ca_certificate(Certificate::from_pem(ca));
        if let Some(name) = &self.server_name {
            cfg = cfg.domain_name(name.clone());
        }
        Ok(cfg)
    }
}

/// Builds the endpoint for one connection attempt. With TLS files the URL
/// must be https; without them (insecure development mode) it must be http.
pub fn endpoint(url: &str, tls: Option<&TlsFiles>) -> Result<Endpoint> {
    let ep = Endpoint::from_shared(url.to_string())
        .with_context(|| format!("bad control plane URL {url:?}"))?;
    match (tls, ep.uri().scheme_str()) {
        (Some(files), Some("https")) => Ok(ep.tls_config(files.load()?)?),
        (Some(_), _) => bail!("control plane URL {url:?} must be https when mTLS is configured"),
        (None, Some("http")) => Ok(ep),
        (None, _) => bail!("control plane URL {url:?} is https but no TLS files are configured"),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;

    fn env(pairs: &[(&str, &str)]) -> impl Fn(&str) -> Option<String> {
        let m: HashMap<String, String> = pairs
            .iter()
            .map(|(k, v)| (k.to_string(), v.to_string()))
            .collect();
        move |k| m.get(k).cloned()
    }

    #[test]
    fn from_env_requires_tls_unless_insecure() {
        assert!(TlsFiles::from_env(env(&[])).is_err());
        assert_eq!(
            TlsFiles::from_env(env(&[("TACHYON_HOSTLINK_INSECURE", "1")])).unwrap(),
            None
        );
        assert!(TlsFiles::from_env(env(&[("TACHYON_TLS_CERT", "c")])).is_err());
        let f = TlsFiles::from_env(env(&[
            ("TACHYON_TLS_CERT", "/etc/tachyon/host.crt"),
            ("TACHYON_TLS_KEY", "/etc/tachyon/host.key"),
            ("TACHYON_TLS_CA", "/etc/tachyon/ca.crt"),
            ("TACHYON_TLS_SERVER_NAME", "cp.internal"),
        ]))
        .unwrap()
        .unwrap();
        assert_eq!(f.cert, PathBuf::from("/etc/tachyon/host.crt"));
        assert_eq!(f.server_name.as_deref(), Some("cp.internal"));
    }

    /// Writes a throwaway CA and a host certificate for `id` into dir.
    fn files(dir: &std::path::Path, id: &str) -> TlsFiles {
        use rcgen::{
            BasicConstraints, CertificateParams, CertifiedIssuer, ExtendedKeyUsagePurpose, IsCa,
            KeyPair, SanType,
        };
        let mut ca_params = CertificateParams::new(Vec::<String>::new()).unwrap();
        ca_params.is_ca = IsCa::Ca(BasicConstraints::Constrained(0));
        let ca = CertifiedIssuer::self_signed(ca_params, KeyPair::generate().unwrap()).unwrap();
        let mut host = CertificateParams::new(Vec::<String>::new()).unwrap();
        host.subject_alt_names = vec![SanType::URI(
            format!("spiffe://tachyon/host/{id}").try_into().unwrap(),
        )];
        host.extended_key_usages = vec![ExtendedKeyUsagePurpose::ClientAuth];
        let key = KeyPair::generate().unwrap();
        let cert = host.signed_by(&key, &ca).unwrap();
        std::fs::write(dir.join("host.crt"), cert.pem()).unwrap();
        std::fs::write(dir.join("host.key"), key.serialize_pem()).unwrap();
        std::fs::write(dir.join("ca.crt"), ca.pem()).unwrap();
        TlsFiles {
            cert: dir.join("host.crt"),
            key: dir.join("host.key"),
            ca: dir.join("ca.crt"),
            server_name: None,
        }
    }

    #[test]
    fn load_reads_files_each_time() {
        let dir = tempfile::tempdir().unwrap();
        let f = files(dir.path(), "h1");
        assert!(f.load().is_ok());
        // Rotation: replacing a file on disk is seen by the next load.
        std::fs::write(&f.key, "garbage").unwrap();
        assert!(f.load().is_err());
        std::fs::remove_file(&f.cert).unwrap();
        let err = format!("{:#}", f.load().unwrap_err());
        assert!(err.contains("client certificate"), "{err}");
    }

    #[test]
    fn endpoint_scheme_must_match_tls() {
        let dir = tempfile::tempdir().unwrap();
        let f = files(dir.path(), "h1");
        assert!(endpoint("https://cp.internal:9090", Some(&f)).is_ok());
        assert!(endpoint("http://cp.internal:9090", Some(&f)).is_err());
        assert!(endpoint("http://localhost:9090", None).is_ok());
        assert!(endpoint("https://cp.internal:9090", None).is_err());
    }
}

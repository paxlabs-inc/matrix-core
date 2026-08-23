# Release Deprecation Notice

## Matrix Protocol Releases — Deprecated

Historical GitHub releases titled **"Matrix Protocol v1.0.0"**, **"Matrix Protocol v0.90.0"**, etc., are **deprecated** effective immediately.

### What This Means

- These releases were created during an incomplete rebrand from the original Matrix Protocol / paxlabs-inc organization to Sidiora Labs / Centra AI branding.
- **The releases remain available** for backward compatibility and existing deployments, but they should not be used for new installations.
- The binary artifacts and tags are unchanged, but the **branding and naming are outdated**.

### Migration Path

**New deployments** should use:

1. Clone from the canonical repository:
   ```bash
   git clone https://github.com/Sidiora-Labs/centra-llm-agents.git
   ```

2. Build from source using current instructions in [README.md](../README.md)

3. Follow the deployment guides in `deploy/` with Centra AI branding

**Existing deployments** can continue to run unchanged. The runtime binaries, environment variables (`MATRIX_*`), HTTP headers (`X-Matrix-*`), Go module paths (`matrix/*`), and URIs (`matrix://`) remain stable per [BRANDING.md](../BRANDING.md).

### Future Releases

Future releases will be titled **"Centra AI v[VERSION]"** and will use Sidiora Labs branding consistently across:

- Release notes and titles
- Documentation and README files
- CHANGELOG entries
- Public-facing copy

The legal license (Centra AI Protocol License) and copyright (© 2026 Sidiora Labs) remain unchanged.

### Why This Matters

Consistent branding is essential for:

- **Trust**: Users and auditors need a clear, consistent product identity
- **Security**: Mismatched branding can signal supply-chain or authenticity issues
- **Clarity**: Mixed Matrix/Centra naming creates confusion about product ownership and support

### Questions?

- **License questions**: See [LICENSE.md](../LICENSE.md) and the License FAQ in [README.md](../README.md)
- **Technical questions**: Open an issue at [github.com/Sidiora-Labs/centra-llm-agents](https://github.com/Sidiora-Labs/centra-llm-agents/issues)
- **Commercial licensing**: Contact `license@Paxeer.app`

---

**Effective Date**: August 23, 2026  
**Applies To**: All releases tagged v0.* through v1.0.0 with "Matrix Protocol" titles

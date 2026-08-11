# Private AgentCore packaging

The per-user Railway image consumes AgentCore as a private, immutable OCI build stage. It does not copy `tmp/agentcore` into Centra AI's Docker context, and the normal Centra AI Docker workflow never checks out the private AgentCore repository. The final stage copies only the pinned Python environment, Node dependencies, browser payload, build identity, managed policy, and license inventory. A BuildKit read-only mount non-editably installs the project into the copied environment and the build rejects editable `.pth` or finder artifacts and imports that still resolve to the source mount.

`agentcore.pin` records four separate identities: the reviewed source revision, the locally loaded image ID, the attested OCI index digest, and the OCI archive checksum. `deploy/railway/Dockerfile` resolves the private GHCR package by the OCI index digest and rejects a runtime whose embedded source revision differs.

To promote a newly reviewed runtime:

1. Prepare the private checkout at `tmp/agentcore`, update the pin file, and run `deploy/codingruntime/build-agentcore.sh`.
2. Authenticate `skopeo` to `ghcr.io/paxlabs-inc/matrix/matrix-agentcore` without placing credentials in command arguments.
3. Run `deploy/codingruntime/publish-agentcore.sh`. It refuses an archive or OCI index that differs from the pin and preserves the attested digest while copying.
4. Run `python3 deploy/codingruntime/verify-packaging.py`, then build the Railway image normally. Existing per-user services remain an operator-controlled staged rollout; these scripts do not deploy or restart them.

The installed Python modules remain inspectable by the AgentCore process that executes them. Centra AI does not claim native-code secrecy. Users are not given the private checkout, image, package, filesystem, or AgentCore endpoint; confidentiality relies on the managed server boundary and browser non-exposure. The MIT license, dependency notices, and Python and Node SBOMs remain in `/usr/share/licenses/matrix-agentcore` in the final image. The read-only Git wrapper is installed separately at `/opt/matrix/coding-bin/git`; the coding supervisor places that directory first on its private PATH.

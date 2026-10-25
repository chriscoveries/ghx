# Fork CI runners

The fork runs Linux build, race tests, vet, formatting, and Windows/Darwin cross
compilation on dedicated self-hosted runners with labels `self-hosted`, `linux`,
and `ghx-ci`. Repository variable `GHX_LINUX_RUNNER` may override the labels with
a JSON array. Upstream keeps its hosted Linux/Windows checks and compatibility
matrix. Main-branch pushes and the daily schedule provide continuing checks.

Register these runners for this repository on isolated build hosts that have no
production data or deployment credentials. An offline or unregistered runner
leaves checks queued; inspect the repository Actions runner page and restore a
runner with the expected labels. Review workflow changes before running them.
The CI token is read-only and checkout does not persist credentials.

# Security

## Reporting a vulnerability

Please report security problems privately, through a GitHub security advisory. Open the repository's Security tab and choose "Report a vulnerability", or go to <https://github.com/feverenergy/pgmem/security/advisories/new>. Don't open a public issue for them.

Say which pgmem version or commit you used, on which OS and CPU, and how to reproduce the problem. We will reply in the advisory and credit you in the fix unless you ask us not to.

The same applies to the engine module in [pgmem-engine](https://github.com/feverenergy/pgmem-engine). Report it here or there.

## What pgmem does not protect against

pgmem is for tests. Some of its behaviour is deliberate and not a vulnerability:

- The server uses trust authentication, and every client is superuser.
- `cmd/pgmem-serve` gives superuser to anyone who can reach its socket or its `-tcp` port. With `-out`, clients can write files in those directories as the user running it.
- A query can use as much memory and CPU as the test process allows.

A bug in PostgreSQL itself belongs with the PostgreSQL project, unless pgmem's patches or kernel cause it.

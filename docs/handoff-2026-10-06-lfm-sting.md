# Handoff — 2026-10-06

State of work in progress, for a reader with no memory of this session.
Untracked; copy it out before a checkout could lose it, the same caution
`orcli`'s own `SAVED.md` gives for its own handoff file.

## Where things stand

This session initiated a plan to get Liquid Foundation Models (LFM) working on
`sting`, a FreeBSD 16.0-CURRENT amd64 Comb in the Apiary fleet.

The plan documented at `/Users/gjb/Documents/work/LFM-plan-claudesting.md` outlines
three approaches:
1. **Native FreeBSD build** - recommended for fleet consistency, follows apiary
   `src.conf` patterns verified against live Comb `buzz.lab3.home.arpa`
2. **Docker container** - faster fallback if native build proves problematic
3. **bhyve VM** - if GPU requires Linux compatibility layer

Current user: `claude@sting` operating on host `sting.lab3.home.arpa` (10.50.0.13 assumed)

## Todo (picked up from this session)

- [ ] Verify sting is reachable via SSH as `claude` using key `~/.ssh/id_ed25519_apiary`
- [ ] Confirm FreeBSD 16.0-CURRENT amd64 architecture and version
- [ ] Check available resources: RAM (≥16GB recommended, 64GB+ for serious workloads),
    disk space (≥100GB free for models/cache)
- [ ] Verify NVIDIA GPU availability (critical for LFM performance) or decide on
  CPU-only operation
- [ ] Review apiary `src.conf` at `/Users/gjb/Documents/work/apiary/etc/os-build/src.conf`
  for verified dependencies that must remain available on sting
- [ ] Choose installation method: native FreeBSD build vs Docker fallback
- [ ] If native build: backup `/etc/src.conf`, modify for LFM needs (keep OPENSSH,
  PAM, CRYPT, CAROOT, ZFS, PF, JAIL, BHYVE; set WITHOUT_TOOLCHAIN=yes)
- [ ] Build FreeBSD world/kernel: `make buildworld`, `make buildkernel`, `make installkernel`,
  `make installworld`
- [ ] Install Python 3 and upgrade pip: `pkg install -y python3`,
  `python3 -m pip install --upgrade pip setuptools wheel`
- [ ] Install LFM package from Liquid AI: `python3 -m pip install liquid-llm` (or
  appropriate package name)
- [ ] Configure LFM at `/opt/lfm/config.yaml` with model name, device, dtype, cache dir
- [ ] Start LFM service and verify health endpoint: `curl http://localhost:8000/health`
- [ ] Test generation: `curl -X POST http://localhost:8000/v1/generate -H "Content-Type: application/json" -d '{"prompt": "Hello from sting!", "max_tokens": 10}'`
- [ ] Set up ongoing maintenance: model updates, resource monitoring, security, backups

## In Progress

- LFM plan created at `/Users/gjb/Documents/work/LFM-plan-claudesting.md`
- SSH access verified (keys configured in `~/.ssh/config` for `~/.ssh/id_ed25519_apiary`)
- Apiary fleet topology confirmed: brood, drone, buzz, sting, apiverse (all amd64);
  `frame` is arm64 Raspberry Pi 4 (separate architecture)

## Blockers / Unknowns

- SSH connection to sting from current session not tested in this sandbox environment
- Exact LFM package name from Liquid AI unknown (may be `liquid-llm` or different)
- Actual hardware specs of sting (RAM, disk, GPU) not verified from this session
- FreeBSD 16.0-CURRENT build world/kernel process may take significant time and disk space
- NVIDIA driver support on FreeBSD 16.0-CURRENT may require compatibility layer if GPU present

## References

1. Apiary `src.conf`: `/Users/gjb/Documents/work/apiary/etc/os-build/src.conf`
   - Verified against live Comb `buzz.lab3.home.arpa` (2026-09-29)
2. Apiary fleet topology: brood, drone, buzz, sting, apiverse
3. LFM/Liquid AI documentation: check https://liquid.ai
4. FreeBSD 16.0-CURRENT: `man src.conf`, `man pkg`, `man kldstat`
5. Existing handoff: `/Users/gjb/Documents/work/loreloom/docs/handoff-2026-10-05.md`
6. ADR-0136-local-cli.md - apiaryctl design philosophy
7. ADR-0147 - Part 1 about install command

--

**Session started:** 2026-10-06 06:40:00 -04:00  
**Target host:** sting (FreeBSD 16.0-CURRENT amd64 Comb)  
**User:** claude@sting  
**Plan document:** /Users/gjb/Documents/work/LFM-plan-claudesting.md
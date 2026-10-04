# Pane deaths

## Summary

The Pi panes that vanished on the nights of 3 and 4 October 2026 were killed by systemd after out-of-memory kills. Nothing in Agency or Pi closed them.

tmux 3.4 starts every pane in its own systemd user scope, `tmux-spawn-<uuid>.scope`. The lanes ran the brood workspace's full `pnpm typecheck` and `pnpm test`, and one run peaks at 25 to 32 GB. The box has 46 GB and no swap, so two lanes validating at once ran it out of memory. The kernel OOM killer then killed one process, a tsc or vitest worker. Scopes default to `OOMPolicy=stop`, so systemd answered by SIGKILLing everything else in that pane's scope, the pane's Pi included. Pi's session file stops mid tool call because Pi got SIGKILL while its `bash` tool was running the validation.

Agency now installs an OOM guard: systemd drop-ins that set `OOMPolicy=continue` on `tmux-spawn-.scope` and `agency.service`. The kernel's victim still dies, the validation command fails with exit 137, and Pi carries on. Every pane death now lands in a log that names the cause, and `agency log deaths` reads it.

## Evidence

Every pane that died overnight has a matching OOM kill in the user journal (`journalctl --user`), a few seconds before the daemon's `pruned dead pane` line in `/tmp/agency-cloud.log`. Times are UTC.

| OOM kill | Pane | Lane | Scope memory peak |
|---|---|---|---|
| 3 Oct 21:53:22 | %1313 | Fagstige Competitor Check | 27.7G |
| 22:09:26 | %1327 | Pack: Båtfører and Løyve | |
| 23:11:50 | %1315 | Fagstige Exam Photos | |
| 23:44:57 | %1330 | Fagstige Pack System v2 | |
| 23:49:13 | %1323 | Pack: Sikkerhetsfaget | 30.9G |
| 23:50:32 | %1325 | Pack: Samfunnskunnskap and Statsborger | |
| 4 Oct 00:10:50 | %1331 | Fagstige Pack System v2 | |
| 00:18:44 | %1340 | Pack: Samfunnskunnskap and Statsborger | |
| 00:20:00 | %1338 | Pack: Sikkerhetsfaget | |
| 00:24:35 | %1334 | Fagstige Competitor Check | 28.1G |
| 00:29:40 | %1342 | Pack: Båtfører and Løyve | |
| 00:40:52 | %1336 | Fagstige Exam Photos | |
| 01:02:16 | %1332 | Brood Ops: Crew, Desk, Feedback | |
| 01:21:05 | %1317 | Pack: Logistikk and Salg | |
| 06:46:07 | %1329 | Pack: Yrkessjåfør and Anleggsmaskinfører | |

The same pattern killed %1296, %1301, %1303 and %1305 earlier on 3 October. Each scope's teardown reads like this:

```
tmux-spawn-2f5fb2c1-….scope: A process of this unit has been killed by the OOM killer.
tmux-spawn-2f5fb2c1-….scope: Killing process 3317461 (pi) with signal SIGKILL.
tmux-spawn-2f5fb2c1-….scope: Killing process 3470591 (pnpm-native) with signal SIGKILL.
tmux-spawn-2f5fb2c1-….scope: Failed with result 'oom-kill'.
tmux-spawn-2f5fb2c1-….scope: Consumed 23min 24.914s CPU time, 27.7G memory peak, 0B memory swap peak.
```

Every Pi session file in the table ends on a `bash` call running `pnpm typecheck`, `pnpm test`, or `pnpm check` across the whole brood workspace. The resumed lanes died the same way within minutes of restarting, each on its next full validation run.

At 23:11:44 the OOM killer also hit a process in `agency.service`'s cgroup. Panes whose scope setup fails stay in the tmux server's cgroup, which is the daemon's. systemd stopped the unit, which killed the daemon (`KillMode=process` spared tmux and the panes), and restarted it at 23:11:55. Agency forgot its tracked panes in that restart, so %1315 has no `pruned` line. %1319, idle since its 22:35 handoff, has none either, so when it ended is unknown.

The other suspects checked out clean:

- **Handoff kills.** `Refusing to kill the active Pi pane` is the dotagents Agency extension refusing a pane's request to kill itself. The successor's later `kill:%1319` at 00:53:11 failed with `can't find pane`, because %1319 was already gone. No kill reached a wrong pane.
- **Agency kills.** `agency kill` and `kill --window` drop the pane from the daemon's tracking as they kill it, so the prune loop never reports those panes. Every dead pane in the table was reported by the prune loop.
- **tmux server or session.** The server stayed up all night: panes from before the deaths, such as %1208 and %1263, are still alive.
- **Pi crashes.** Every session ends mid tool call with no error entry, which fits SIGKILL and nothing else.
- **Cloud sync and viewers.** Viewers live on Earth's tmux server and never touch the box's panes.

`%1358` and `%1359` vanished at 00:53 and 00:57 without an OOM kill. Neither ever wrote to the activity diary; they were short-lived command panes.

## The OOM guard

On start, the daemon writes two drop-ins when a systemd user manager runs, and reloads systemd when either changed. The reload applies the policy to scopes that already exist.

```
~/.config/systemd/user/tmux-spawn-.scope.d/50-agency-oom.conf    [Scope]   OOMPolicy=continue
~/.config/systemd/user/agency.service.d/50-agency-oom.conf       [Service] OOMPolicy=continue
```

Check a live pane with `systemctl --user show <tmux-spawn-….scope> -p OOMPolicy`.

The guard keeps an OOM kill to the kernel's chosen victim. Memory still runs out when lanes validate in parallel: brood's full `pnpm test` and `pnpm typecheck` need a concurrency cap, or lanes need to take turns, before the box stops OOM-killing test workers. That work belongs to brood.

## The death log

`~/.agents/run/agency/pane-deaths.jsonl` holds one JSON line per event. It rotates into `pane-deaths.jsonl.1` at 4 MiB. The daemon and the tmux hooks write to it under a lock.

| Event | Written by | Carries |
|---|---|---|
| `killed` | the daemon, for `agency kill`, `kill --window`, phone kills, viewer drops, spawn rollbacks | target pid, label, role, group, directory, command; the requesting pane, role, pid, and command line |
| `died` | the tmux `pane-died` hook | pid, exit status or signal, label, role, group, start directory, command, free memory, systemd OOM evidence |
| `exited` | the tmux `pane-exited` hook | pane id; tmux reports nothing else once a clean pane closes |
| `gone` | the daemon's 5 second prune | label, role, and command of a tracked pane that disappeared |
| `handoff` | `agency replace` | old pane, successor pane, pid, label |
| `daemon` | daemon start | daemon pid, headless or interactive |
| `guard` | daemon start, when the drop-ins changed | the units that got `OOMPolicy=continue` |

Both tmux configs set `remain-on-exit failed`, so a pane that exits non-zero or by signal stays dead until the `pane-died` hook has read its pid and status. The hook, `agency pane-exit died <socket> <pane>`, then kills the pane. It needs no daemon, so deaths during a daemon restart still get recorded. For SIGKILL deaths it reads `journalctl --user` for the minute before: systemd naming the pane's pid while stopping an OOM-killed unit is an exact match, and any other OOM kill in that minute is a probable one.

`kill-pane`, `kill-window`, and `kill-session` from tmux itself fire no hook. Those panes show up as `gone`, and the reader calls them vanished.

## Reading it

```
agency log deaths                     # the last 48 hours
agency log deaths --since 7d          # 90m, 48h, 7d, or all
agency log deaths --pane %1313        # one pane, including handoffs to or from it
agency log deaths --json              # stories as JSON, for agents
agency log deaths -f                  # follow new deaths as they settle
```

The reader folds each death's events into one story: the hook's `died` and the prune's `gone` a few seconds later, or a `handoff` and the successor's kill hours later. It adds the pane's last activity diary step before the end, which is usually the command it was running.

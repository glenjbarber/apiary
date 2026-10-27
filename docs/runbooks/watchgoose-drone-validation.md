# Validate Watchgoose heartbeats on Drone

Use this procedure to verify that Drone's `managerd` sends its configured
Watchgoose heartbeat, that missed pings alert, and that reporting recovers.
It pauses only the Watchgoose sender; it does not interrupt Drone's network,
Raft participation, or workloads. Do not use Sting for this exercise.

## Before starting

1. Confirm Drone is the only Hive whose Watchgoose settings you will change.
   In **Machine → Operations → Watchgoose monitoring**, check that monitoring
   is enabled and the configured ping URL is the intended Drone check. The
   complete URL is secret; do not copy it into notes, shell history, or logs.
2. Confirm the Watchgoose check is configured with a period longer than the
   configured Apiary heartbeat interval, and know its grace time. Auto-created
   slug checks use Watchgoose defaults (one-day period and one-hour grace), so
   configure a suitable period and grace in Watchgoose before relying on an
   auto-created check for this exercise.
3. Record the start time, current Watchgoose status, Apiary heartbeat interval,
   Watchgoose period, and grace time. Make sure you can reach the Watchgoose
   dashboard independently of Drone.

## Check normal reporting

1. Restart `managerd` on Drone if its Watchgoose settings have just changed;
   the settings are loaded at startup.
2. Wait for at least two heartbeat intervals. Confirm Watchgoose records
   successful pings for the Drone check and that its status remains healthy.
3. Check Drone's `managerd` logs for heartbeat failures or recovery messages.
   The sender does not log the ping URL.

## Trigger and recover a missed-heartbeat alert

Run these commands on Drone as an administrator. The marker is checked by
`managerd` before each ping. Creating it suppresses only future Watchgoose
requests; removing it resumes reporting on the next heartbeat interval.

```sh
sudo install -d -m 0755 /var/run/apiary
sudo touch /var/run/apiary/watchgoose.pause
```

1. Record the time the marker was created. Leave Drone's network and services
   running. Wait for the Watchgoose check period plus grace time, then confirm
   the missed-heartbeat alert and record when it fired.
2. Resume reporting:

   ```sh
   sudo rm /var/run/apiary/watchgoose.pause
   ```

3. Wait up to one heartbeat interval. Confirm Watchgoose records a successful
   ping and marks the check recovered. Record the recovery time.
4. If the alert does not fire or recovery does not appear, check the configured
   period/grace, the Drone service logs, and whether the pause marker is absent.
   Do not change Watchgoose configuration on other Hives as part of diagnosis.

## Interpretation

A successful ping confirms that `managerd` reached Watchgoose over HTTPS. It
does not establish that Drone's guests, services, or overall Hive are healthy.
The current sender uses a GET success ping and has no separate management API
or check-creation logic. A configured slug URL with `?create=1` can still trigger
Watchgoose's own auto-provisioning on the first ping. The sender retries
transient server/network errors up to three times and logs failures and later
recovery without including the secret URL.

Watchgoose's ping API accepts GET for UUID and slug URLs. Slug-based URLs use a
project Ping Key and can optionally create missing checks with `?create=1`;
UUID URLs identify an existing check directly. Apiary accepts both forms, but
the complete URL remains a secret in either case. See the [Pinging API](https://watchgoose.com/docs/http_api/),
[Auto Provisioning](https://watchgoose.com/docs/autoprovisioning/), and
[Configuring Checks](https://watchgoose.com/docs/configuring_checks/) docs.

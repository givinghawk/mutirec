# Timetables

Ready-to-import timetable files for MutiRec. Load one from the app via
**Timetable → Import from file** — both the app's own RFC3339 export format and
the compact `[year, month, day, hour, minute, name?]` per-stage format used by
these files are accepted.

These are distributed as attachments on each
[GitHub release](https://github.com/givinghawk/mutirec/releases) as well; they
are **not** bundled into the application binary or the container image, so a
default install ships with no timetable.

## Files

- `defqon1-2026.json` — a community-contributed stage/set schedule.

## Notes on content and trademarks

These files are community-contributed convenience data describing publicly
announced event schedules. MutiRec is an independent tool and is not
affiliated with, endorsed by, or sponsored by any festival, promoter, artist,
or event organizer named in these files; those names belong to their
respective owners. Use this data only where you have the right to, and only as
a scheduling aid for recordings you are permitted to make.

## Adding your own

Any JSON in either accepted shape works. The compact shape is the easiest to
hand-author:

```json
[
  {
    "stage": "RED",
    "url": "https://www.youtube.com/@example/live",
    "sets": [
      [2026, 6, 26, 13, 0, "Opening Ceremony"],
      [2026, 6, 26, 14, 0, "Next Artist"],
      [2026, 6, 26, 15, 0]
    ]
  }
]
```

Each set row is `[year, month, day, hour, minute, name]`. A row with no name
just marks the end time of the previous set. After-midnight hours are handled
automatically (a `25:00`, or a `1:00` authored on the following day's row, both
resolve to 1 AM the next morning).

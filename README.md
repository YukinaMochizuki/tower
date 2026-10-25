# tower

The control layer for development environments. workstream says what it wants
(a box of repository X at ref Y, a machine of this blueprint); tower decides
where it runs, when its golden image is rebuilt, what is cached, and what is
reclaimed when space runs low, and makes it so on a target: a hangar
endpoint, or a host running `hostd`.

`docs/architecture.md` is the map; `AGENTS.md` is the guide for working in
this repository.

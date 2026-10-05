#!/bin/sh
# One frame, then idle: proves the bar costs nothing when nothing changes.
printf '\033[32mneferbar\033[0m \xef\x83\xa7 ready\n'
exec sleep 100000

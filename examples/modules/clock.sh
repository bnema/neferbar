#!/bin/sh
# One frame per second: the time, with a Nerd Font clock icon.
while :; do
	printf '\033[1;38;2;137;180;250m\xef\x80\x97 \033[0m%s\n' "$(date +%H:%M:%S)"
	sleep 1
done

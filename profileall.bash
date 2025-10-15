#!/usr/bin/env bash
# Dump profiles from a running grene instance.
# usage: ./profileall.bash http://my.grene.protocol.instance: e.g,
# ./profileall.bash https://grene-protocol.com/api
if [ "$#" -ne 1 ]; then
	echo 'expected a command-line argument: URL'
	echo "usage: $0 http://my.grene.protocol.instance"
	exit 1
fi
echo 'profiling CPU for 30s... please wait'
for profile in 'heap' 'goroutine' 'threadcreate' 'block' 'mutex' 'profile'; do
	curl -sf $1/v1/debug/pprof/$profile >"$profile.pprof" && echo "$profile: ok" &
done
wait

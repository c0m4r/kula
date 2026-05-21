#!/bin/bash

cd "$(dirname "$0")"
wget -nv -O roles/kula/files/kula.deb https://github.com/c0m4r/kula/releases/download/0.16.0/kula-0.16.0-amd64.deb
wget -nv -O roles/kula/files/kula.rpm https://github.com/c0m4r/kula/releases/download/0.16.0/kula-0.16.0-x86_64.rpm

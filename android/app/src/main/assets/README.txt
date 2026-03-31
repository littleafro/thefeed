Place Android client binaries in this folder before building.

Supported filenames (service picks by device ABI):
- thefeed-client-arm64
- thefeed-client-armv7
- thefeed-client-x86_64

Fallback filename (legacy, any ABI):
- thefeed-client

How to produce arm64 binary from project root:
- make build-android-arm64
- cp build/thefeed-client-android-arm64 android/app/src/main/assets/thefeed-client-arm64

The app copies this file to internal storage, marks it executable, and runs it as:
- --data-dir <app files dir>/thefeeddata
- --port 8080

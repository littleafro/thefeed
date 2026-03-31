Place the Android client binary in this folder before building:

Filename required:
- thefeed-client

How to produce it from project root:
- make build-android-arm64
- cp build/thefeed-client-android-arm64 android/app/src/main/assets/thefeed-client

The app copies this file to internal storage, marks it executable, and runs it as:
- --data-dir <app files dir>/thefeeddata
- --port 8080

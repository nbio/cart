# cart
Fetcher of build artifacts from Circle CI.

## Install
`go get github.com/nbio/cart`

## Get an artifact from the latest green build of `master`

``` console
$ cart username/repo path/to/artifact
```

Authentication uses `$CIRCLE_TOKEN` in your shell's environment or the `-token` flag on the command line.

## Get an artifact from a specific branch

``` console
$ cart -branch feature1 username/repo path/to/artifact
```

## Get an artifact from a specific build number

``` console
$ cart -build 42 username/repo path/to/artifact
```

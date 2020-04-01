# Go Code Generation Demo

This repository contains the example code for the [Generating Go code in Kubebuilder style](https://banzaicloud.com/blog/generating-go-code) post.


## Usage

```bash
git clone git@github.com:banzaicloud/go-code-generation-demo.git
cd go-code-generation-demo
go build -o shallowcopy
chmod +x shallowcopy
./shallowcopy shallowcopy paths=./example output:artifacts:config=
cat example/zz_generated.shallowcopy.go
```

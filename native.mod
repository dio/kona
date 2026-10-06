module github.com/dio/kona

go 1.27.1

require (
	github.com/dio/egtest v0.0.0-20261003031549-ef81a9226df9
	github.com/envoyproxy/envoy/source/extensions/dynamic_modules v0.0.0-20260423231439-f1dd21b16c24
)

require gopkg.in/yaml.v3 v3.0.1 // indirect

replace github.com/envoyproxy/envoy/source/extensions/dynamic_modules => github.com/dio/envoy/source/extensions/dynamic_modules v0.0.0-20260909102307-0a804c57cf5f

all: simpterm

simpterm: main.go go.mod go.sum
	go build -o $@ .

clean:
	rm -f simpterm

.PHONY: all clean

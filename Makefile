CC ?= cc
CFLAGS ?= -O2 -Wall -Wextra -pedantic
LDFLAGS ?=

all: simpterm

simpterm: simpterm.c
	$(CC) $(CFLAGS) -o $@ $< $(LDFLAGS)

clean:
	rm -f simpterm

.PHONY: all clean

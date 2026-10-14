# Included after a library's own Makefile: builds its objects only (WASI has
# no shared libraries) and lists them, with absolute paths, in
# $(PGMEM_OBJS_LIST).
PGMEM_OBJS_LIST ?= pgmem-objs.txt
.PHONY: pgmem-objs
pgmem-objs: $(OBJS)
	@echo $(addprefix $(CURDIR)/,$(OBJS)) >$(PGMEM_OBJS_LIST)

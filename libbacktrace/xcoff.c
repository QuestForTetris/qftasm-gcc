/* xcoff.c -- Get debug data from an XCOFF file for backtraces.
   Copyright (C) 2012-2017 Free Software Foundation, Inc.
   Adapted from elf.c.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are
met:

    (1) Redistributions of source code must retain the above copyright
    notice, this list of conditions and the following disclaimer.

    (2) Redistributions in binary form must reproduce the above copyright
    notice, this list of conditions and the following disclaimer in
    the documentation and/or other materials provided with the
    distribution.

    (3) The name of the author may not be used to
    endorse or promote products derived from this software without
    specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE AUTHOR ``AS IS'' AND ANY EXPRESS OR
IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE IMPLIED
WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
DISCLAIMED. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR ANY DIRECT,
INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES
(INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR
SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION)
HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT,
STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING
IN ANY WAY OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE
POSSIBILITY OF SUCH DAMAGE.  */

#include "config.h"

#include <errno.h>
#include <stdlib.h>
#include <string.h>
#include <sys/types.h>

#ifdef HAVE_LOADQUERY
#include <sys/ldr.h>
#endif

#include "backtrace.h"
#include "internal.h"

/* The configure script must tell us whether we are 32-bit or 64-bit
   XCOFF.  We could make this code test and support either possibility,
   but there is no point.  This code only works for the currently
   running executable, which means that we know the XCOFF mode at
   configure time.  */

#if BACKTRACE_XCOFF_SIZE != 32 && BACKTRACE_XCOFF_SIZE != 64
#error "Unknown BACKTRACE_XCOFF_SIZE"
#endif

/* XCOFF file header.  */

#if BACKTRACE_XCOFF_SIZE == 32

typedef struct {
  uint16_t f_magic;
  uint16_t f_nscns;
  uint32_t f_timdat;
  uint32_t f_symptr;
  uint32_t f_nsyms;
  uint16_t f_opthdr;
  uint16_t f_flags;
} b_xcoff_filhdr;

#define XCOFF_MAGIC	0737

#else /* BACKTRACE_XCOFF_SIZE != 32 */

typedef struct {
  uint16_t f_magic;
  uint16_t f_nscns;
  uint32_t f_timdat;
  uint64_t f_symptr;
  uint16_t f_opthdr;
  uint16_t f_flags;
  uint32_t f_nsyms;
} b_xcoff_filhdr;

#define XCOFF_MAGIC	0767

#endif /* BACKTRACE_XCOFF_SIZE != 32 */

#define F_SHROBJ	0x2000	/* File is a shared object.  */

/* XCOFF section header.  */

#if BACKTRACE_XCOFF_SIZE == 32

typedef struct {
  char s_name[8];
  uint32_t s_paddr;
  uint32_t s_vaddr;
  uint32_t s_size;
  uint32_t s_scnptr;
  uint32_t s_relptr;
  uint32_t s_lnnoptr;
  uint16_t s_nreloc;
  uint16_t s_nlnno;
  uint32_t s_flags;
} b_xcoff_scnhdr;

#define _OVERFLOW_MARKER	65535

#else /* BACKTRACE_XCOFF_SIZE != 32 */

typedef struct {
  char name[8];
  uint64_t s_paddr;
  uint64_t s_vaddr;
  uint64_t s_size;
  uint64_t s_scnptr;
  uint64_t s_relptr;
  uint64_t s_lnnoptr;
  uint32_t s_nreloc;
  uint32_t s_nlnno;
  uint32_t s_flags;
} b_xcoff_scnhdr;

#endif /* BACKTRACE_XCOFF_SIZE != 32 */

#define STYP_TEXT	0x20	/* Executable text (code) section.  */
#define STYP_OVRFLO	0x8000	/* Line-number field overflow section.  */

/* XCOFF symbol.  */

#define SYMNMLEN	8

#if BACKTRACE_XCOFF_SIZE == 32

typedef struct {
  union {
    char _name[SYMNMLEN];
    struct {
      uint32_t _zeroes;
      uint32_t _offset;
    } _s;
  } _u;
#define n_name		_u._name
#define n_zeroes	_u._s._zeroes
#define n_offset_	_u._s._offset

  uint32_t n_value;
  int16_t  n_scnum;
  uint16_t n_type;
  uint8_t  n_sclass;
  uint8_t  n_numaux;
} __attribute__ ((packed)) b_xcoff_syment;

#else /* BACKTRACE_XCOFF_SIZE != 32 */

typedef struct {
  uint64_t n_value;
  uint32_t n_offset_;
  int16_t  n_scnum;
  uint16_t n_type;
  uint8_t  n_sclass;
  uint8_t  n_numaux;
} __attribute__ ((packed)) b_xcoff_syment;

#endif /* BACKTRACE_XCOFF_SIZE != 32 */

#define SYMESZ	18

#define C_EXT		2	/* External symbol.  */
#define C_FCN		101	/* Beginning or end of function.  */
#define C_FILE		103	/* Source file name.  */
#define C_HIDEXT	107	/* Unnamed external symbol.  */
#define C_BINCL		108	/* Beginning of include file.  */
#define C_EINCL		109	/* End of include file.  */
#define C_WEAKEXT	111	/* Weak external symbol.  */

#define ISFCN(x)	((x) & 0x0020)

/* XCOFF AUX entry.  */

#define AUXESZ		18
#define FILNMLEN	14

typedef union {
#if BACKTRACE_XCOFF_SIZE == 32
  struct {
    uint16_t pad;
    uint16_t x_lnnohi;
    uint16_t x_lnno;
  } x_block;
#else
  struct {
    uint32_t x_lnno;
  } x_block;
#endif
  union {
    char x_fname[FILNMLEN];
    struct {
      uint32_t x_zeroes;
      uint32_t x_offset;
      char     pad[FILNMLEN-8];
      uint8_t  x_ftype;
    } _x;
  } x_file;
#if BACKTRACE_XCOFF_SIZE == 32
  struct {
    uint32_t x_exptr;
    uint32_t x_fsize;
    uint32_t x_lnnoptr;
    uint32_t x_endndx;
  } x_fcn;
#else
  struct {
    uint64_t x_lnnoptr;
    uint32_t x_fsize;
    uint32_t x_endndx;
  } x_fcn;
#endif
  struct {
    uint8_t pad[AUXESZ-1];
    uint8_t x_auxtype;
  } x_auxtype;
} __attribute__ ((packed)) b_xcoff_auxent;

/* XCOFF line number entry.  */

#if BACKTRACE_XCOFF_SIZE == 32

typedef struct {
  union {
    uint32_t l_symndx;
    uint32_t l_paddr;
  } l_addr;
  uint16_t l_lnno;
} b_xcoff_lineno;

#define LINESZ	6

#else /* BACKTRACE_XCOFF_SIZE != 32 */

typedef struct {
  union {
    uint32_t l_symndx;
    uint64_t l_paddr;
  } l_addr;
  uint32_t l_lnno;
} b_xcoff_lineno;

#define LINESZ	12

#endif /* BACKTRACE_XCOFF_SIZE != 32 */

#if BACKTRACE_XCOFF_SIZE == 32
#define XCOFF_AIX_TEXTBASE	0x10000000u
#else
#define XCOFF_AIX_TEXTBASE	0x100000000ul
#endif

/* AIX big archive fixed-length header.  */

#define AIAMAGBIG	"<bigaf>\n"

typedef struct {
  char fl_magic[8];	/* Archive magic string.  */
  char fl_memoff[20];	/* Offset to member table.  */
  char fl_gstoff[20];	/* Offset to global symbol table.  */
  char fl_gst64off[20];	/* Offset to global symbol table for 64-bit objects.  */
  char fl_fstmoff[20];	/* Offset to first archive member.  */
  char fl_freeoff[20];	/* Offset to first member on free list.  */
} b_ar_fl_hdr;

/* AIX big archive file member header.  */

typedef struct {
  char ar_size[20];	/* File member size - decimal.  */
  char ar_nxtmem[20];	/* Next member offset - decimal.  */
  char ar_prvmem[20];	/* Previous member offset - decimal.  */
  char ar_date[12];	/* File member date - decimal.  */
  char ar_uid[12];	/* File member userid - decimal.  */
  char ar_gid[12];	/* File member group id - decimal.  */
  char ar_mode[12];	/* File member mode - octal.  */
  char ar_namlen[4];	/* File member name length - decimal.  */
  char ar_name[2];	/* Start of member name.  */
} b_ar_hdr;


/* Information we keep for an XCOFF symbol.  */

struct xcoff_symbol
{
  /* The name of the symbol.  */
  const char *name;
  /* The address of the symbol.  */
  uintptr_t address;
  /* The size of the symbol.  */
  size_t size;
};

/* Information to pass to xcoff_syminfo.  */

struct xcoff_syminfo_data
{
  /* Symbols for the next module.  */
  struct xcoff_syminfo_data *next;
  /* The XCOFF symbols, sorted by address.  */
  struct xcoff_symbol *symbols;
  /* The number of symbols.  */
  size_t count;
};

/* Information about an include file.  */

struct xcoff_incl
{
  /* File name.  */
  const char *filename;
  /* Offset to first line number from the include file.  */
  uintptr_t begin;
  /* Offset to last line number from the include file.  */
  uintptr_t end;
};

/* A growable vector of include files information.  */

struct xcoff_incl_vector
{
  /* Memory.  This is an array of struct xcoff_incl.  */
  struct backtrace_vector vec;
  /* Number of include files.  */
  size_t count;
};

/* Map a single PC value to a file/function/line.  */

struct xcoff_line
{
  /* PC.  */
  uintptr_t pc;
  /* File name.  Many entries in the array are expected to point to
     the same file name.  */
  const char *filename;
  /* Function name.  */
  const char *function;
  /* Line number.  */
  int lineno;
};

/* A growable vector of line number information.  This is used while
   reading the line numbers.  */

struct xcoff_line_vector
{
  /* Memory.  This is an array of struct xcoff_line.  */
  struct backtrace_vector vec;
  /* Number of valid mappings.  */
  size_t count;
};

/* The information we need to map a PC to a file and line.  */

struct xcoff_fileline_data
{
  /* The data for the next file we know about.  */
  struct xcoff_fileline_data *next;
  /* Line number information.  */
  struct xcoff_line_vector vec;
};


/* A dummy callback function used when we can't find any debug info.  */

static int
xcoff_nodebug (struct backtrace_state *state ATTRIBUTE_UNUSED,
	       uintptr_t pc ATTRIBUTE_UNUSED,
	       backtrace_full_callback callback ATTRIBUTE_UNUSED,
	       backtrace_error_callback error_callback, void *data)
{
  error_callback (data, "no debug info in XCOFF executable", -1);
  return 0;
}

/* A dummy callback function used when we can't find a symbol
   table.  */

static void
xcoff_nosyms (struct backtrace_state *state ATTRIBUTE_UNUSED,
	      uintptr_t addr ATTRIBUTE_UNUSED,
	      backtrace_syminfo_callback callback ATTRIBUTE_UNUSED,
	      backtrace_error_callback error_callback, void *data)
{
  error_callback (data, "no symbol table in XCOFF executable", -1);
}

/* Compare struct xcoff_symbol for qsort.  */

static int
xcoff_symbol_compare (const void *v1, const void *v2)
{
  const struct xcoff_symbol *e1 = (const struct xcoff_symbol *) v1;
  const struct xcoff_symbol *e2 = (const struct xcoff_symbol *) v2;

  if (e1->address < e2->address)
    return -1;
  else if (e1->address > e2->address)
    return 1;
  else
    return 0;
}

/* Compare an ADDR against an xcoff_symbol for bsearch.  */

static int
xcoff_symbol_search (const void *vkey, const void *ventry)
{
  const uintptr_t *key = (const uintptr_t *) vkey;
  const struct xcoff_symbol *entry = (const struct xcoff_symbol *) ventry;
  uintptr_t addr;

  addr = *key;
  if (addr < entry->address)
    return -1;
  else if ((entry->size == 0 && addr > entry->address)
	   || (entry->size > 0 && addr >= entry->address + entry->size))
    return 1;
  else
    return 0;
}

/* Add XDATA to the list in STATE.  */

static void
xcoff_add_syminfo_data (struct backtrace_state *state,
			struct xcoff_syminfo_data *xdata)
{
  if (!state->threaded)
    {
      struct xcoff_syminfo_data **pp;

      for (pp = (struct xcoff_syminfo_data **) (void *) &state->syminfo_data;
	   *pp != NULL;
	   pp = &(*pp)->next)
	;
      *pp = xdata;
    }
  else
    {
      while (1)
	{
	  struct xcoff_syminfo_data **pp;

	  pp = (struct xcoff_syminfo_data **) (void *) &state->syminfo_data;

	  while (1)
	    {
	      struct xcoff_syminfo_data *p;

	      p = backtrace_atomic_load_pointer (pp);

	      if (p == NULL)
		break;

	      pp = &p->next;
	    }

	  if (__sync_bool_compare_and_swap (pp, NULL, xdata))
	    break;
	}
    }
}

/* Return the symbol name and value for an ADDR.  */

static void
xcoff_syminfo (struct backtrace_state *state ATTRIBUTE_UNUSED, uintptr_t addr,
	       backtrace_syminfo_callback callback,
	       backtrace_error_callback error_callback ATTRIBUTE_UNUSED,
	       void *data)
{
  struct xcoff_syminfo_data *edata;
  struct xcoff_symbol *sym = NULL;

  if (!state->threaded)
    {
      for (edata = (struct xcoff_syminfo_data *) state->syminfo_data;
	   edata != NULL;
	   edata = edata->next)
	{
	  sym = ((struct xcoff_symbol *)
		 bsearch (&addr, edata->symbols, edata->count,
			  sizeof (struct xcoff_symbol), xcoff_symbol_search));
	  if (sym != NULL)
	    break;
	}
    }
  else
    {
      struct xcoff_syminfo_data **pp;

      pp = (struct xcoff_syminfo_data **) (void *) &state->syminfo_data;
      while (1)
	{
	  edata = backtrace_atomic_load_pointer (pp);
	  if (edata == NULL)
	    break;

	  sym = ((struct xcoff_symbol *)
		 bsearch (&addr, edata->symbols, edata->count,
			  sizeof (struct xcoff_symbol), xcoff_symbol_search));
	  if (sym != NULL)
	    break;

	  pp = &edata->next;
	}
    }

  if (sym == NULL)
    callback (data, addr, NULL, 0, 0);
  else
    callback (data, addr, sym->name, sym->address, sym->size);
}

/* Return the name of an XCOFF symbol.  */

static const char *
xcoff_symname (const b_xcoff_syment *asym,
	       const unsigned char *strtab, size_t strtab_size)
{
#if BACKTRACE_XCOFF_SIZE == 32
  if (asym->n_zeroes != 0)
    {
      /* Make a copy as we will release the symtab view.  */
      char name[SYMNMLEN+1];
      strncpy (name, asym->n_name, SYMNMLEN);
      name[SYMNMLEN] = '\0';
      return strdup (name);
    }
#endif
  if (asym->n_sclass & 0x80)
    return NULL; /* .debug */
  if (asym->n_offset_ >= strtab_size)
    return NULL;
  return (const char *) strtab + asym->n_offset_;
}

/* Initialize the symbol table info for xcoff_syminfo.  */

static int
xcoff_initialize_syminfo (struct backtrace_state *state,
			  uintptr_t base_address,
			  const b_xcoff_scnhdr *sects,
			  const b_xcoff_syment *syms, size_t nsyms,
			  const unsigned char *strtab, size_t strtab_size,
			  backtrace_error_callback error_callback, void *data,
			  struct xcoff_syminfo_data *sdata)
{
  size_t xcoff_symbol_count;
  size_t xcoff_symbol_size;
  struct xcoff_symbol *xcoff_symbols;
  size_t i;
  unsigned int j;

  /* We only care about function symbols.  Count them.  */
  xcoff_symbol_count = 0;
  for (i = 0; i < nsyms; ++i)
    {
      const b_xcoff_syment *asym = &syms[i];
      if ((asym->n_sclass == C_EXT || asym->n_sclass == C_HIDEXT
	    || asym->n_sclass == C_WEAKEXT)
	  && ISFCN (asym->n_type) && asym->n_numaux > 0 && asym->n_scnum > 0)
	++xcoff_symbol_count;

      i += asym->n_numaux;
    }

  xcoff_symbol_size = xcoff_symbol_count * sizeof (struct xcoff_symbol);
  xcoff_symbols = ((struct xcoff_symbol *)
		   backtrace_alloc (state, xcoff_symbol_size, error_callback,
				    data));
  if (xcoff_symbols == NULL)
    return 0;

  j = 0;
  for (i = 0; i < nsyms; ++i)
    {
      const b_xcoff_syment *asym = &syms[i];
      if ((asym->n_sclass == C_EXT || asym->n_sclass == C_HIDEXT
	    || asym->n_sclass == C_WEAKEXT)
	  && ISFCN (asym->n_type) && asym->n_numaux > 0 && asym->n_scnum > 0)
	{
	  const b_xcoff_auxent *aux = (const b_xcoff_auxent *) (asym + 1);
	  xcoff_symbols[j].name = xcoff_symname (asym, strtab, strtab_size);
	  xcoff_symbols[j].address = base_address + asym->n_value
				   - sects[asym->n_scnum - 1].s_paddr;
	  /* x_fsize will be 0 if there is no debug information.  */
	  xcoff_symbols[j].size = aux->x_fcn.x_fsize;
	  ++j;
	}

      i += asym->n_numaux;
    }

  backtrace_qsort (xcoff_symbols, xcoff_symbol_count,
		   sizeof (struct xcoff_symbol), xcoff_symbol_compare);

  sdata->next = NULL;
  sdata->symbols = xcoff_symbols;
  sdata->count = xcoff_symbol_count;

  return 1;
}

/* Compare struct xcoff_line for qsort.  */

static int
xcoff_line_compare (const void *v1, const void *v2)
{
  const struct xcoff_line *ln1 = (const struct xcoff_line *) v1;
  const struct xcoff_line *ln2 = (const struct xcoff_line *) v2;

  if (ln1->pc < ln2->pc)
    return -1;
  else if (ln1->pc > ln2->pc)
    return 1;
  else
    return 0;
}

/* Find a PC in a line vector.  We always allocate an extra entry at
   the end of the lines vector, so that this routine can safely look
   at the next entry.  */

static int
xcoff_line_search (const void *vkey, const void *ventry)
{
  const uintptr_t *key = (const uintptr_t *) vkey;
  const struct xcoff_line *entry = (const struct xcoff_line *) ventry;
  uintptr_t pc;

  pc = *key;
  if (pc < entry->pc)
    return -1;
  else if ((entry + 1)->pc == (uintptr_t) -1 || pc >= (entry + 1)->pc)
    return 1;
  else
    return 0;
}

/* Look for a PC in the line vector for one module.  On success,
   call CALLBACK and return whatever it returns.  On error, call
   ERROR_CALLBACK and return 0.  Sets *FOUND to 1 if the PC is found,
   0 if not.  */

static int
xcoff_lookup_pc (struct backtrace_state *state ATTRIBUTE_UNUSED,
		 struct xcoff_fileline_data *fdata, uintptr_t pc,
		 backtrace_full_callback callback,
		 backtrace_error_callback error_callback ATTRIBUTE_UNUSED,
		 void *data, int *found)
{
  const struct xcoff_line *ln;
  const char *function;

  *found = 1;

  ln = (struct xcoff_line *) bsearch (&pc, fdata->vec.vec.base,
				      fdata->vec.count,
				      sizeof (struct xcoff_line),
				      xcoff_line_search);
  if (ln == NULL)
    {
      *found = 0;
      return 0;
    }

  function = ln->function;
  /* AIX prepends a '.' to function entry points, remove it.  */
  if (*function == '.')
    ++function;
  return callback (data, pc, ln->filename, ln->lineno, function);
}

/* Return the file/line information for a PC using the XCOFF lineno
   mapping we built earlier.  */

static int
xcoff_fileline (struct backtrace_state *state, uintptr_t pc,
		backtrace_full_callback callback,
		backtrace_error_callback error_callback, void *data)

{
  struct xcoff_fileline_data *fdata;
  int found;
  int ret;

  if (!state->threaded)
    {
      for (fdata = (struct xcoff_fileline_data *) state->fileline_data;
	   fdata != NULL;
	   fdata = fdata->next)
	{
	  ret = xcoff_lookup_pc (state, fdata, pc, callback, error_callback,
				 data, &found);
	  if (ret != 0 || found)
	    return ret;
	}
    }
  else
    {
      struct xcoff_fileline_data **pp;

      pp = (struct xcoff_fileline_data **) (void *) &state->fileline_data;
      while (1)
	{
	  fdata = backtrace_atomic_load_pointer (pp);
	  if (fdata == NULL)
	    break;

	  ret = xcoff_lookup_pc (state, fdata, pc, callback, error_callback,
				 data, &found);
	  if (ret != 0 || found)
	    return ret;

	  pp = &fdata->next;
	}
    }

  /* FIXME: See if any libraries have been dlopen'ed.  */

  return callback (data, pc, NULL, 0, NULL);
}

/* Add a new mapping to the vector of line mappings that we are
   building.  Returns 1 on success, 0 on failure.  */

static int
xcoff_add_line (struct backtrace_state *state, uintptr_t pc,
		const char *filename, const char *function, uint32_t lnno,
		backtrace_error_callback error_callback, void *data,
		struct xcoff_line_vector *vec)
{
  struct xcoff_line *ln;

  ln = ((struct xcoff_line *)
	backtrace_vector_grow (state, sizeof (struct xcoff_line),
			       error_callback, data, &vec->vec));
  if (ln == NULL)
    return 0;

  ln->pc = pc;
  ln->filename = filename;
  ln->function = function;
  ln->lineno = lnno;

  ++vec->count;

  return 1;
}

/* Add the line number entries for a function to the line vector.  */

static int
xcoff_process_linenos (struct backtrace_state *state, uintptr_t base_address,
		       const b_xcoff_syment *fsym, const char *filename,
		       const b_xcoff_scnhdr *sects,
		       const unsigned char *strtab, size_t strtab_size,
		       uint32_t fcn_lnno, struct xcoff_incl_vector *vec,
		       struct xcoff_line_vector *lvec,
		       const unsigned char *linenos, size_t linenos_size,
		       uintptr_t lnnoptr0,
		       backtrace_error_callback error_callback, void *data)
{
  const b_xcoff_auxent *aux;
  const b_xcoff_lineno *lineno;
  const unsigned char *lineptr;
  const char *function;
  struct xcoff_incl *incl = NULL;
  uintptr_t lnnoptr;
  uintptr_t pc;
  uint32_t lnno;
  int begincl;
  size_t i;

  aux = (const b_xcoff_auxent *) (fsym + 1);
  lnnoptr = aux->x_fcn.x_lnnoptr;

  if (lnnoptr < lnnoptr0 || lnnoptr + LINESZ > lnnoptr0 + linenos_size)
    return 0;

  function = xcoff_symname (fsym, strtab, strtab_size);
  if (function == NULL)
    return 0;

  /* Skip first entry that points to symtab.  */

  lnnoptr += LINESZ;

  lineptr = linenos + (lnnoptr - lnnoptr0);

  begincl = -1;
  while (lineptr + LINESZ <= linenos + linenos_size)
    {
      lineno = (const b_xcoff_lineno *) lineptr;

      lnno = lineno->l_lnno;
      if (lnno == 0)
	  break;

      /* If part of a function other than the beginning comes from an
	 include file, the line numbers are absolute, rather than
	 relative to the beginning of the function.  */
      for (i = 0; i < vec->count; ++i)
	{
	  incl = (struct xcoff_incl *) vec->vec.base + i;
	  if (incl->begin <= lnnoptr && lnnoptr <= incl->end)
	    break;
	}
      if (begincl == -1)
	begincl = (i < vec->count);
      if (i < vec->count)
	{
	  filename = incl->filename;
	  if (begincl == 1)
	    lnno += fcn_lnno - 1;
	}
      else
	lnno += fcn_lnno - 1;

      pc = base_address + lineno->l_addr.l_paddr
	 - sects[fsym->n_scnum - 1].s_paddr;
      xcoff_add_line (state, pc, filename, function, lnno, error_callback,
		      data, lvec);

      lnnoptr += LINESZ;
      lineptr += LINESZ;
    }

  return 1;
}

/* Initialize the line vector info for xcoff_fileline.  */

static int
xcoff_initialize_fileline (struct backtrace_state *state,
			   uintptr_t base_address,
			   const b_xcoff_scnhdr *sects,
			   const b_xcoff_syment *syms, size_t nsyms,
			   const unsigned char *strtab, size_t strtab_size,
			   const unsigned char *linenos, size_t linenos_size,
			   uint64_t lnnoptr0,
			   backtrace_error_callback error_callback, void *data)
{
  struct xcoff_fileline_data *fdata;
  struct xcoff_incl_vector vec;
  struct xcoff_line *ln;
  const b_xcoff_syment *fsym;
  const b_xcoff_auxent *aux;
  const char *filename;
  const char *name;
  struct xcoff_incl *incl;
  uintptr_t begin, end;
  uintptr_t lnno;
  size_t i;

  fdata = ((struct xcoff_fileline_data *)
	   backtrace_alloc (state, sizeof (struct xcoff_fileline_data),
			    error_callback, data));
  if (fdata == NULL)
    return 0;

  memset (fdata, 0, sizeof *fdata);
  memset (&vec, 0, sizeof vec);

  /* Process include files first.  */

  begin = 0;
  for (i = 0; i < nsyms; ++i)
    {
      const b_xcoff_syment *asym = &syms[i];

      switch (asym->n_sclass)
	{
	  case C_BINCL:
	    begin = asym->n_value;
	    break;

	  case C_EINCL:
	    if (begin == 0)
	      break;
	    end = asym->n_value;
	    incl = ((struct xcoff_incl *)
		    backtrace_vector_grow (state, sizeof (struct xcoff_incl),
					   error_callback, data, &vec.vec));
	    if (incl != NULL)
	      {
		incl->filename = xcoff_symname (asym, strtab, strtab_size);
		incl->begin = begin;
		incl->end = end;
		++vec.count;
	      }
	    begin = 0;
	    break;
	}

      i += asym->n_numaux;
    }

  filename = NULL;
  fsym = NULL;
  for (i = 0; i < nsyms; ++i)
    {
      const b_xcoff_syment *asym = &syms[i];

      switch (asym->n_sclass)
	{
	  case C_FILE:
	    filename = xcoff_symname (asym, strtab, strtab_size);
	    if (filename == NULL)
	      break;

	    /* If the file auxiliary entry is not used, the symbol name is
	       the name of the source file. If the file auxiliary entry is
	       used, then the symbol name should be .file, and the first
	       file auxiliary entry (by convention) contains the source
	       file name.  */

	    if (asym->n_numaux > 0 && !strcmp (filename, ".file"))
	      {
		aux = (const b_xcoff_auxent *) (asym + 1);
		if (aux->x_file._x.x_zeroes != 0)
		  {
		    /* Make a copy as we will release the symtab view.  */
		    char name[FILNMLEN+1];
		    strncpy (name, aux->x_file.x_fname, FILNMLEN);
		    name[FILNMLEN] = '\0';
		    filename = strdup (name);
		  }
		else if (aux->x_file._x.x_offset < strtab_size)
		  filename = (const char *) strtab + aux->x_file._x.x_offset;
		else
		  filename = NULL;
	      }
	    break;

	  case C_EXT:
	  case C_HIDEXT:
	  case C_WEAKEXT:
	    fsym = NULL;
	    if (!ISFCN (asym->n_type) || asym->n_numaux == 0)
	      break;
	    if (filename == NULL)
	      break;
	    fsym = asym;
	    break;

	  case C_FCN:
	    if (asym->n_numaux == 0)
	      break;
	    if (fsym == NULL)
	      break;
	    name = xcoff_symname (asym, strtab, strtab_size);
	    if (name == NULL)
	      break;
	    aux = (const b_xcoff_auxent *) (asym + 1);
#if BACKTRACE_XCOFF_SIZE == 32
	    lnno = (uint32_t) aux->x_block.x_lnnohi << 16
		 | aux->x_block.x_lnno;
#else
	    lnno = aux->x_block.x_lnno;
#endif
	    if (!strcmp (name, ".bf"))
	      {
		xcoff_process_linenos (state, base_address, fsym, filename,
				       sects, strtab, strtab_size, lnno, &vec,
				       &fdata->vec, linenos, linenos_size,
				       lnnoptr0, error_callback, data);
	      }
	    else if (!strcmp (name, ".ef"))
	      {
		fsym = NULL;
	      }
	    break;
	}

      i += asym->n_numaux;
    }

  /* Allocate one extra entry at the end.  */
  ln = ((struct xcoff_line *)
	backtrace_vector_grow (state, sizeof (struct xcoff_line),
			       error_callback, data, &fdata->vec.vec));
  if (ln == NULL)
    goto fail;
  ln->pc = (uintptr_t) -1;
  ln->filename = NULL;
  ln->function = NULL;
  ln->lineno = 0;

  if (!backtrace_vector_release (state, &fdata->vec.vec, error_callback, data))
    goto fail;

  backtrace_qsort (fdata->vec.vec.base, fdata->vec.count,
		   sizeof (struct xcoff_line), xcoff_line_compare);

  if (!state->threaded)
    {
      struct xcoff_fileline_data **pp;

      for (pp = (struct xcoff_fileline_data **) (void *) &state->fileline_data;
	   *pp != NULL;
	   pp = &(*pp)->next)
	;
      *pp = fdata;
    }
  else
    {
      while (1)
	{
	  struct xcoff_fileline_data **pp;

	  pp = (struct xcoff_fileline_data **) (void *) &state->fileline_data;

	  while (1)
	    {
	      struct xcoff_fileline_data *p;

	      p = backtrace_atomic_load_pointer (pp);

	      if (p == NULL)
		break;

	      pp = &p->next;
	    }

	  if (__sync_bool_compare_and_swap (pp, NULL, fdata))
	    break;
	}
    }

  return 1;

fail:
  return 0;
}

/* Add the backtrace data for one XCOFF file.  Returns 1 on success,
   0 on failure (in both cases descriptor is closed).  */

static int
xcoff_add (struct backtrace_state *state, int descriptor, off_t offset,
	   uintptr_t base_address, backtrace_error_callback error_callback,
	   void *data, fileline *fileline_fn, int *found_sym, int exe)
{
  struct backtrace_view fhdr_view;
  struct backtrace_view sects_view;
  struct backtrace_view linenos_view;
  struct backtrace_view syms_view;
  struct backtrace_view str_view;
  b_xcoff_filhdr fhdr;
  const b_xcoff_scnhdr *sects;
  const b_xcoff_scnhdr *stext;
  uint64_t lnnoptr;
  uint32_t nlnno;
  off_t str_off;
  size_t sects_size;
  size_t syms_size;
  int32_t str_size;
  int sects_view_valid;
  int linenos_view_valid;
  int syms_view_valid;
  int str_view_valid;
  int magic_ok;
  int i;

  *found_sym = 0;

  sects_view_valid = 0;
  linenos_view_valid = 0;
  syms_view_valid = 0;
  str_view_valid = 0;

  /* Map the XCOFF file header.  */
  if (!backtrace_get_view (state, descriptor, offset, sizeof (b_xcoff_filhdr),
			   error_callback, data, &fhdr_view))
    goto fail;

  memcpy (&fhdr, fhdr_view.data, sizeof fhdr);
  magic_ok = (fhdr.f_magic == XCOFF_MAGIC);

  backtrace_release_view (state, &fhdr_view, error_callback, data);

  if (!magic_ok)
    {
      if (exe)
        error_callback (data, "executable file is not XCOFF", 0);
      goto fail;
    }

  /* Verify object is of expected type.  */
  if ((exe && (fhdr.f_flags & F_SHROBJ))
      || (!exe && !(fhdr.f_flags & F_SHROBJ)))
    goto fail;

  /* Read the section headers.  */

  sects_size = fhdr.f_nscns * sizeof (b_xcoff_scnhdr);

  if (!backtrace_get_view (state, descriptor,
			   offset + sizeof (fhdr) + fhdr.f_opthdr,
			   sects_size, error_callback, data, &sects_view))
    goto fail;
  sects_view_valid = 1;
  sects = (const b_xcoff_scnhdr *) sects_view.data;

  /* FIXME: assumes only one .text section.  */
  for (i = 0; i < fhdr.f_nscns; ++i)
      if ((sects[i].s_flags & 0xffff) == STYP_TEXT)
	  break;
  if (i == fhdr.f_nscns)
    goto fail;

  stext = &sects[i];

  /* AIX ldinfo_textorg includes the XCOFF headers.  */
  base_address = (exe ? XCOFF_AIX_TEXTBASE : base_address) + stext->s_scnptr;

  lnnoptr = stext->s_lnnoptr;
  nlnno = stext->s_nlnno;

#if BACKTRACE_XCOFF_SIZE == 32
  if (nlnno == _OVERFLOW_MARKER)
    {
      int sntext = i + 1;
      /* Find the matching .ovrflo section.  */
      for (i = 0; i < fhdr.f_nscns; ++i)
	{
	    if (((sects[i].s_flags & 0xffff) == STYP_OVRFLO)
		&& sects[i].s_nlnno == sntext)
	      {
		nlnno = sects[i].s_vaddr;
		break;
	      }
	}
    }
#endif

  /* Read the symbol table and the string table.  */

  if (fhdr.f_symptr != 0)
    {
      struct xcoff_syminfo_data *sdata;

      /* Symbol table is followed by the string table.  The string table
	 starts with its length (on 4 bytes).
	 Map the symbol table and the length of the string table.  */
      syms_size = fhdr.f_nsyms * sizeof (b_xcoff_syment);

      if (!backtrace_get_view (state, descriptor, offset + fhdr.f_symptr,
			       syms_size + 4, error_callback, data,
			       &syms_view))
	goto fail;
      syms_view_valid = 1;

      memcpy (&str_size, syms_view.data + syms_size, 4);

      str_off = fhdr.f_symptr + syms_size;

      if (str_size > 4)
	{
	  /* Map string table (including the length word).  */

	  if (!backtrace_get_view (state, descriptor, offset + str_off,
				   str_size, error_callback, data, &str_view))
	    goto fail;
	  str_view_valid = 1;
	}

      sdata = ((struct xcoff_syminfo_data *)
	       backtrace_alloc (state, sizeof *sdata, error_callback, data));
      if (sdata == NULL)
	goto fail;

      if (!xcoff_initialize_syminfo (state, base_address, sects,
				     syms_view.data, fhdr.f_nsyms,
				     str_view.data, str_size,
				     error_callback, data, sdata))
	{
	  backtrace_free (state, sdata, sizeof *sdata, error_callback, data);
	  goto fail;
	}

      *found_sym = 1;

      xcoff_add_syminfo_data (state, sdata);
    }

  /* Read the line number entries.  */

  if (fhdr.f_symptr != 0 && lnnoptr != 0)
    {
      size_t linenos_size = (size_t) nlnno * LINESZ;

      if (!backtrace_get_view (state, descriptor, offset + lnnoptr,
			       linenos_size,
			       error_callback, data, &linenos_view))
	goto fail;
      linenos_view_valid = 1;

      if (xcoff_initialize_fileline (state, base_address, sects,
				     syms_view.data, fhdr.f_nsyms,
				     str_view.data, str_size,
				     linenos_view.data, linenos_size,
				     lnnoptr, error_callback, data))
	*fileline_fn = xcoff_fileline;

      backtrace_release_view (state, &linenos_view, error_callback, data);
      linenos_view_valid = 0;
    }

  backtrace_release_view (state, &sects_view, error_callback, data);
  sects_view_valid = 0;
  if (syms_view_valid)
    backtrace_release_view (state, &syms_view, error_callback, data);
  syms_view_valid = 0;

  /* We've read all we need from the executable.  */
  if (!backtrace_close (descriptor, error_callback, data))
    goto fail;
  descriptor = -1;

  return 1;

 fail:
  if (sects_view_valid)
    backtrace_release_view (state, &sects_view, error_callback, data);
  if (str_view_valid)
    backtrace_release_view (state, &str_view, error_callback, data);
  if (syms_view_valid)
    backtrace_release_view (state, &syms_view, error_callback, data);
  if (linenos_view_valid)
    backtrace_release_view (state, &linenos_view, error_callback, data);
  if (descriptor != -1 && offset == 0)
    backtrace_close (descriptor, error_callback, data);
  return 0;
}

#ifdef HAVE_LOADQUERY

/* Read an integer value in human-readable format from an AIX
   big archive fixed-length or member header.  */

static int
xcoff_parse_decimal (const char *buf, size_t size, off_t *off)
{
  char str[32];
  char *end;

  if (size >= sizeof str)
    return 0;
  memcpy (str, buf, size);
  str[size] = '\0';
  *off = strtol (str, &end, 10);
  if (*end != '\0' && *end != ' ')
    return 0;

  return 1;
}

/* Add the backtrace data for a member of an AIX big archive.
   Returns 1 on success, 0 on failure.  */

static int
xcoff_armem_add (struct backtrace_state *state, int descriptor,
		 uintptr_t base_address, const char *member,
		 backtrace_error_callback error_callback, void *data,
		 fileline *fileline_fn, int *found_sym)
{
  struct backtrace_view view;
  b_ar_fl_hdr fl_hdr;
  const b_ar_hdr *ar_hdr;
  off_t off;
  off_t len;
  int memlen;

  *found_sym = 0;

  /* Map archive fixed-length header.  */

  if (!backtrace_get_view (state, descriptor, 0, sizeof (b_ar_fl_hdr),
			   error_callback, data, &view))
    goto fail;

  memcpy (&fl_hdr, view.data, sizeof (b_ar_fl_hdr));

  backtrace_release_view (state, &view, error_callback, data);

  if (memcmp (fl_hdr.fl_magic, AIAMAGBIG, 8) != 0)
    goto fail;

  memlen = strlen (member);

  /* Read offset of first archive member.  */
  if (!xcoff_parse_decimal (fl_hdr.fl_fstmoff, sizeof fl_hdr.fl_fstmoff, &off))
    goto fail;
  while (off != 0)
    {
      /* Map archive member header and member name.  */

      if (!backtrace_get_view (state, descriptor, off,
			       sizeof (b_ar_hdr) + memlen,
			       error_callback, data, &view))
	break;

      ar_hdr = (const b_ar_hdr *) view.data;

      /* Read archive member name length.  */
      if (!xcoff_parse_decimal (ar_hdr->ar_namlen, sizeof ar_hdr->ar_namlen,
				&len))
	{
	  backtrace_release_view (state, &view, error_callback, data);
	  break;
	}
      if (len == memlen && !memcmp (ar_hdr->ar_name, member, memlen))
	{
	  off = (off + sizeof (b_ar_hdr) + memlen + 1) & ~1;

	  /* The archive can contain several members with the same name
	     (e.g. 32-bit and 64-bit), so continue if not ok.  */

	  if (xcoff_add (state, descriptor, off, base_address, error_callback,
			 data, fileline_fn, found_sym, 0))
	    {
	      backtrace_release_view (state, &view, error_callback, data);
	      return 1;
	    }
	}

      /* Read offset of next archive member.  */
      if (!xcoff_parse_decimal (ar_hdr->ar_nxtmem, sizeof ar_hdr->ar_nxtmem,
				&off))
	{
	  backtrace_release_view (state, &view, error_callback, data);
	  break;
	}
      backtrace_release_view (state, &view, error_callback, data);
    }

 fail:
  /* No matching member found.  */
  backtrace_close (descriptor, error_callback, data);
  return 0;
}

/* Add the backtrace data for dynamically loaded libraries.  */

static void
xcoff_add_shared_libs (struct backtrace_state *state,
		       backtrace_error_callback error_callback,
		       void *data, fileline *fileline_fn, int *found_sym)
{
  const struct ld_info *ldinfo;
  void *buf;
  unsigned int buflen;
  const char *member;
  int descriptor;
  int does_not_exist;
  int lib_found_sym;
  int ret;

  /* Retrieve the list of loaded libraries.  */

  buf = NULL;
  buflen = 512;
  do
    {
      buf = realloc (buf, buflen);
      if (buf == NULL)
	{
	  ret = -1;
	  break;
	}
      ret = loadquery (L_GETINFO, buf, buflen);
      if (ret == 0)
	break;
      buflen *= 2;
    }
  while (ret == -1 && errno == ENOMEM);
  if (ret != 0)
    {
      free (buf);
      return;
    }

  ldinfo = (const struct ld_info *) buf;
  while ((const char *) ldinfo < (const char *) buf + buflen)
    {
      if (*ldinfo->ldinfo_filename != '/')
	goto next;

      descriptor = backtrace_open (ldinfo->ldinfo_filename, error_callback,
				   data, &does_not_exist);
      if (descriptor < 0)
	goto next;

      /* Check if it is an archive (member name not empty).  */

      member = ldinfo->ldinfo_filename + strlen (ldinfo->ldinfo_filename) + 1;
      if (*member)
	{
	  xcoff_armem_add (state, descriptor,
			   (uintptr_t) ldinfo->ldinfo_textorg, member,
			   error_callback, data, fileline_fn, &lib_found_sym);
	}
      else
	{
	  xcoff_add (state, descriptor, 0, (uintptr_t) ldinfo->ldinfo_textorg,
		     error_callback, data, fileline_fn, &lib_found_sym, 0);
	}
      if (lib_found_sym)
	*found_sym = 1;

 next:
      if (ldinfo->ldinfo_next == 0)
	break;
      ldinfo = (const struct ld_info *) ((const char *) ldinfo
					 + ldinfo->ldinfo_next);
    }

    free (buf);
}
#endif /* HAVE_LOADQUERY */

/* Initialize the backtrace data we need from an XCOFF executable.
   Returns 1 on success, 0 on failure.  */

int
backtrace_initialize (struct backtrace_state *state,
		      const char *filename ATTRIBUTE_UNUSED, int descriptor,
		      backtrace_error_callback error_callback,
		      void *data, fileline *fileline_fn)
{
  int ret;
  int found_sym;
  fileline xcoff_fileline_fn = xcoff_nodebug;

  ret = xcoff_add (state, descriptor, 0, 0, error_callback, data,
		   &xcoff_fileline_fn, &found_sym, 1);
  if (!ret)
    return 0;

#ifdef HAVE_LOADQUERY
  xcoff_add_shared_libs (state, error_callback, data, &xcoff_fileline_fn,
			 &found_sym);
#endif

  if (!state->threaded)
    {
      if (found_sym)
	state->syminfo_fn = xcoff_syminfo;
      else if (state->syminfo_fn == NULL)
	state->syminfo_fn = xcoff_nosyms;
    }
  else
    {
      if (found_sym)
	backtrace_atomic_store_pointer (&state->syminfo_fn, xcoff_syminfo);
      else
	__sync_bool_compare_and_swap (&state->syminfo_fn, NULL, xcoff_nosyms);
    }

  if (!state->threaded)
    {
      if (state->fileline_fn == NULL || state->fileline_fn == xcoff_nodebug)
	*fileline_fn = xcoff_fileline_fn;
    }
  else
    {
      fileline current_fn;

      current_fn = backtrace_atomic_load_pointer (&state->fileline_fn);
      if (current_fn == NULL || current_fn == xcoff_nodebug)
	*fileline_fn = xcoff_fileline_fn;
    }

  return 1;
}

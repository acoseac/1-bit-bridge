// Write a DSDIFF file's DIIN chunk with TagLib 2 (ExtractorVersion 20,
// backlog B140): the Edited Master Information chunk's title (DITI) and
// artist (DIAR), laid out as TagLib lays them out, which is the DSDIFF 1.5
// layout: a 4-byte big-endian count, then the text, in ISO-8859-1.
//
// Usage: taglib_dff_diin <file.dff> <title> <artist>
//
// Saves the DIIN tag alone and strips nothing, so an ID3 chunk the file
// already carries stays where it is.
#include <taglib/dsdiffdiintag.h>
#include <taglib/dsdifffile.h>
#include <taglib/tstring.h>

#include <iostream>

int main(int argc, char **argv) {
  if (argc != 4) {
    std::cerr << "usage: taglib_dff_diin <file.dff> <title> <artist>" << std::endl;
    return 2;
  }
  TagLib::DSDIFF::File f(argv[1]);
  if (!f.isValid()) {
    std::cerr << argv[1] << ": TagLib does not read it as DSDIFF" << std::endl;
    return 1;
  }
  TagLib::DSDIFF::DIIN::Tag *diin = f.DIINTag(true);
  diin->setTitle(TagLib::String(argv[2], TagLib::String::UTF8));
  diin->setArtist(TagLib::String(argv[3], TagLib::String::UTF8));
  if (!f.save(TagLib::DSDIFF::File::DIIN, TagLib::File::StripNone)) {
    std::cerr << argv[1] << ": save failed" << std::endl;
    return 1;
  }
  return 0;
}

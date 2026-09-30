// Write a DSDIFF file's tags with TagLib 2 (ExtractorVersion 20, backlog
// B140).
//
//   taglib_dff_diin diin <file.dff> <title> <artist>
//     Saves the DIIN (Edited Master Information) chunk's title (DITI) and
//     artist (DIAR), laid out as TagLib lays them out, which is the DSDIFF 1.5
//     layout: a 4-byte big-endian count, then the text, in ISO-8859-1. Saves
//     the DIIN tag alone and strips nothing, so an ID3 chunk the file already
//     carries stays where it is.
//
//   taglib_dff_diin id3-title <file.dff> <title>
//     Sets the ID3 tag's title and saves the ID3 tag alone. TagLib rewrites the
//     tag where it read it: a root "ID3 " chunk, or one nested in PROP, the
//     placement TagLib's reader accepts beside the root one (a root tag wins
//     where the file holds both).
#include <taglib/dsdiffdiintag.h>
#include <taglib/dsdifffile.h>
#include <taglib/id3v2tag.h>
#include <taglib/tstring.h>

#include <iostream>
#include <string>

int main(int argc, char **argv) {
  if (argc < 4) {
    std::cerr << "usage: taglib_dff_diin diin <file.dff> <title> <artist>" << std::endl
              << "       taglib_dff_diin id3-title <file.dff> <title>" << std::endl;
    return 2;
  }
  std::string mode = argv[1];
  TagLib::DSDIFF::File f(argv[2]);
  if (!f.isValid()) {
    std::cerr << argv[2] << ": TagLib does not read it as DSDIFF" << std::endl;
    return 1;
  }
  bool ok = false;
  if (mode == "diin" && argc == 5) {
    TagLib::DSDIFF::DIIN::Tag *diin = f.DIINTag(true);
    diin->setTitle(TagLib::String(argv[3], TagLib::String::UTF8));
    diin->setArtist(TagLib::String(argv[4], TagLib::String::UTF8));
    ok = f.save(TagLib::DSDIFF::File::DIIN, TagLib::File::StripNone);
  } else if (mode == "id3-title" && argc == 4) {
    if (!f.hasID3v2Tag()) {
      std::cerr << argv[2] << ": TagLib finds no ID3 tag to rewrite" << std::endl;
      return 1;
    }
    f.ID3v2Tag()->setTitle(TagLib::String(argv[3], TagLib::String::UTF8));
    ok = f.save(TagLib::DSDIFF::File::ID3v2, TagLib::File::StripNone);
  } else {
    std::cerr << "bad arguments" << std::endl;
    return 2;
  }
  if (!ok) {
    std::cerr << argv[2] << ": save failed" << std::endl;
    return 1;
  }
  return 0;
}

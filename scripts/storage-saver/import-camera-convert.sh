#!/usr/bin/env bash
# Workstation-only equivalent of jpeg-to-avif.sh with two ExifTool invocations.
set -Eeuo pipefail
die() { printf '%s: %s\n' "${0##*/}" "$*" >&2; exit 1; }
[[ $# -eq 2 ]] || die "usage: ${0##*/} INPUT_JPEG OUTPUT_AVIF"
input=$1
output=$2
[[ -f $input && -r $input ]] || die "input JPEG is not readable: $input"
[[ $output == *.avif ]] || die 'output must use the .avif extension'
for command in exiftool avifenc avifdec quiet-exec realpath setsid perl; do
    command -v "$command" >/dev/null 2>&1 || die "required command not found: $command"
done
[[ $(realpath -m "$input") != "$(realpath -m "$output")" ]] || die 'input and output paths must differ'
jobs=${AVIF_JOBS:-1}
[[ $jobs =~ ^[1-9][0-9]*$ ]] || die 'AVIF_JOBS must be a positive integer'
output_dir=$(dirname "$output")
[[ -d $output_dir && -w $output_dir ]] || die "output directory is not writable: $output_dir"
work_dir=$(mktemp -d "$output_dir/.import-avif.XXXXXX")
trap 'rm -rf -- "$work_dir"' EXIT

# JSON::PP and MIME::Base64 ship with the existing image's Perl. No jq,
# dependency installation, encoder change, or eval of image metadata is needed.
cat > "$work_dir/metadata.pl" <<'PERL'
use strict;
use warnings;
use JSON::PP qw(decode_json);
use MIME::Base64 qw(decode_base64 encode_base64);
use Encode qw(encode);
sub fail { die "import-camera-convert: $_[0]\n"; }
sub load_json {
    my ($file) = @_;
    open my $handle, '<:raw', $file or fail("cannot read metadata: $!");
    local $/; my $data = decode_json(<$handle>);
    ref($data) eq 'ARRAY' && @$data == 1 && ref($data->[0]) eq 'HASH'
        or fail('invalid ExifTool JSON');
    return $data->[0];
}
sub scalar_tag {
    my ($record, $tag) = @_;
    return '' unless defined $record->{$tag};
    ref($record->{$tag}) and fail("unexpected structured metadata: $tag");
    return "$record->{$tag}";
}
sub binary_tag {
    my ($record, $tag) = @_;
    my $value = scalar_tag($record, $tag);
    if ($value =~ s/^base64://) {
        $value =~ /\A(?:[A-Za-z0-9+\/]{4})*(?:[A-Za-z0-9+\/]{2}==|[A-Za-z0-9+\/]{3}=)?\z/
            or fail("invalid binary metadata: $tag");
        my $bytes = decode_base64($value);
        encode_base64($bytes, '') eq $value or fail("noncanonical binary metadata: $tag");
        return $bytes;
    }
    # ExifTool emits valid textual binary values (notably XMP XML) as JSON text.
    return encode('UTF-8', $value);
}
my ($mode, $source_file, $output_file, $info_file) = @ARGV;
my $source = load_json($source_file);
scalar_tag($source, 'FileType') eq 'JPEG' or fail('input is not a JPEG');
if (scalar_tag($source, 'MPImage2') ne '' || scalar_tag($source, 'MPImageCount') ne '') {
    fail('unsupported multi-picture JPEG (MPF)');
}
if (binary_tag($source, 'XMP') =~ /GCamera:MicroVideo|GCamera:MotionPhoto|Camera:MotionPhoto|hdrgm:|urn:com:apple:photo:2020:aux:hdrgainmap/) {
    fail('unsupported motion or HDR JPEG');
}
my $width = scalar_tag($source, 'ImageWidth');
my $height = scalar_tag($source, 'ImageHeight');
$width =~ /\A[1-9][0-9]*\z/ && $height =~ /\A[1-9][0-9]*\z/
    or fail('unable to determine JPEG dimensions');
my $orientation = scalar_tag($source, 'Orientation');
$orientation eq '' || $orientation =~ /\A[1-8]\z/ or fail('unsupported EXIF orientation value');
# Validate binary representation even when the source needs no color profile.
my $source_icc = binary_tag($source, 'ICC_Profile');
exit 0 if $mode eq 'source';
$mode eq 'compare' or fail('unknown metadata validation mode');
my $output = load_json($output_file);
for my $tag (qw(DateTimeOriginal OffsetTimeOriginal GPSLatitude GPSLongitude)) {
    my $before = scalar_tag($source, $tag);
    $before eq '' || $before eq scalar_tag($output, $tag)
        or fail("metadata validation failed for $tag");
}
if (length $source_icc) {
    $source_icc eq binary_tag($output, 'ICC_Profile')
        or fail('AVIF output lost or changed the source ICC profile');
}
open my $handle, '<:raw', $info_file or fail("cannot read AVIF info: $!");
local $/; my $info = <$handle>;
my ($resolution) = $info =~ /^ \* Resolution\s*:\s*([^\r\n]+)/m;
defined($resolution) or fail('missing AVIF dimensions');
$resolution =~ s/\s+$//;
$resolution eq "${width}x${height}" or fail('dimension validation failed');
$info =~ /^ \* Format\s*: YUV420\r?$/m or fail('AVIF output is not 4:2:0');
for my $label ('Color Primaries', 'Transfer Char.', 'Matrix Coeffs.') {
    $info =~ /^ \* \Q$label\E\s*:/m or fail("AVIF output has no $label signal");
}
my %rotations = (3 => 2, 5 => 1, 8 => 1, 6 => 3, 7 => 3);
my %mirrors = (2 => 1, 4 => 0, 5 => 0, 7 => 0);
if (exists $rotations{$orientation}) {
    my $rotation = $rotations{$orientation};
    $info =~ /^\s*\* irot \(Rotation\)\s*: $rotation\r?$/m
        or fail('EXIF orientation was not represented as the expected irot transform');
}
if (exists $mirrors{$orientation}) {
    my $mirror = $mirrors{$orientation};
    $info =~ /^\s*\* imir \(Mirror\)\s*: $mirror /m
        or fail('EXIF orientation was not represented as the expected imir transform');
}
PERL

tags=(-FileType -ImageWidth -ImageHeight -DateTimeOriginal -OffsetTimeOriginal
      -GPSLatitude -GPSLongitude -Orientation -MPImageCount -MPImage2 -XMP -ICC_Profile)
exiftool -q -q -q -json -n -b "${tags[@]}" "$input" > "$work_dir/source.json"
perl "$work_dir/metadata.pl" source "$work_dir/source.json"
temporary_avif="$work_dir/avif-q50-s6-420-v1.avif"
quiet-exec avifenc --qcolor 50 --jobs "$jobs" --speed 6 --yuv 420 "$input" "$temporary_avif"
[[ -s $temporary_avif ]] || die 'avifenc did not produce an output file'
avifdec -i "$temporary_avif" > "$work_dir/avif.info"
exiftool -q -q -q -json -n -b "${tags[@]}" "$temporary_avif" > "$work_dir/output.json"
perl "$work_dir/metadata.pl" compare "$work_dir/source.json" "$work_dir/output.json" "$work_dir/avif.info"
mv -f -- "$temporary_avif" "$output"

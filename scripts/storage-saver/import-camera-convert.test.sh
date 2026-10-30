#!/usr/bin/env bash
set -Eeuo pipefail
here=$(cd -- "$(dirname -- "$0")" && pwd)
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
perl - "$here/import-camera-convert.sh" "$work" <<'PERL'
use strict;
use warnings;
use JSON::PP qw(encode_json decode_json);
use MIME::Base64 qw(encode_base64);
my ($script, $work) = @ARGV;
open my $fh, '<', $script or die $!;
local $/; my $script_body = <$fh>;
my ($validator) = $script_body =~ /<<'PERL'\n(.*?)\nPERL\n/s;
defined($validator) or die 'cannot extract validator';
sub write_file { my ($name,$value)=@_; open my $f,'>:raw',"$work/$name" or die $!; print {$f} $value; close $f or die $!; }
write_file('metadata.pl',$validator);
my $icc='base64:'.encode_base64("\x00ICC\xffprofile",'');
my $base={FileType=>'JPEG',ImageWidth=>5184,ImageHeight=>3888,DateTimeOriginal=>'2023:04:08 18:14:53',OffsetTimeOriginal=>'+02:00',GPSLatitude=>41.123456789,GPSLongitude=>12.987654321,ICC_Profile=>$icc};
my $info=" * Resolution     : 5184x3888\n * Format         : YUV420\n * Color Primaries: 1\n * Transfer Char. : 13\n * Matrix Coeffs.  : 6\n";
my $passed=0;
sub check {
    my ($label,$source,$output,$avif_info,$want,$pattern,$mode)=@_;
    write_file('source.json',encode_json([$source]));write_file('output.json',encode_json([$output]));write_file('avif.info',$avif_info);
    my $pid=fork();defined($pid) or die $!;
    if(!$pid){open STDERR,'>',"$work/error" or die $!;exec 'perl',"$work/metadata.pl",($mode//'compare'),"$work/source.json","$work/output.json","$work/avif.info";die $!;}
    waitpid($pid,0);my $ok=$?==0;
    open my $e,'<',"$work/error" or die $!;my $error=<$e>//'';
    $ok==$want && (!$pattern || $error=~$pattern) or die "FAIL $label: $error";
    $passed++;
}
check('identity',$base,$base,$info,1);
for my $tag (qw(DateTimeOriginal OffsetTimeOriginal GPSLatitude GPSLongitude ICC_Profile)) {
    my %changed=%$base;$changed{$tag}=$tag eq 'ICC_Profile'?'base64:YWJj':'different';
    check("changed $tag",$base,\%changed,$info,0,qr/metadata validation failed|ICC profile/);
    delete $changed{$tag};check("lost $tag",$base,\%changed,$info,0,qr/metadata validation failed|ICC profile/);
}
for my $tag (qw(MPImage2 MPImageCount)) {check($tag,{%$base,$tag=>'base64:YWJj'},$base,$info,0,qr/unsupported multi-picture JPEG/,'source');}
for my $marker ('GCamera:MicroVideo','GCamera:MotionPhoto','Camera:MotionPhoto','hdrgm:','urn:com:apple:photo:2020:aux:hdrgainmap') {
    for my $xmp ($marker,'base64:'.encode_base64($marker,'')) {check('motion/HDR',{%$base,XMP=>$xmp},$base,$info,0,qr/unsupported motion or HDR JPEG/,'source');}
}
check('bad ICC',{%$base,ICC_Profile=>'base64:not!base64'},$base,$info,0,qr/invalid binary metadata/,'source');
check('bad dimensions',{%$base,ImageWidth=>0},$base,$info,0,qr/determine JPEG dimensions/,'source');
check('bad orientation',{%$base,Orientation=>9},$base,$info,0,qr/unsupported EXIF orientation/,'source');
check('wrong dimensions',$base,$base,$info=~s/5184x3888/3888x5184/r,0,qr/dimension validation/);
check('wrong subsampling',$base,$base,$info=~s/YUV420/YUV444/r,0,qr/not 4:2:0/);
for my $label ('Color Primaries','Transfer Char.','Matrix Coeffs.') {
    my $missing=join('',grep {index($_,$label)<0} split /(?<=\n)/,$info);
    check('missing color signal',$base,$base,$missing,0,qr/has no/);
}
my %rot=(3=>2,5=>1,8=>1,6=>3,7=>3);my %mir=(2=>1,4=>0,5=>0,7=>0);
for my $orientation (1..8) {
    my $transforms='';$transforms.=" * irot (Rotation) : $rot{$orientation}\n" if exists $rot{$orientation};
    $transforms.=" * imir (Mirror) : $mir{$orientation} (axis)\n" if exists $mir{$orientation};
    check("orientation $orientation",{%$base,Orientation=>$orientation},$base,$info.$transforms,1);
    check("lost orientation $orientation",{%$base,Orientation=>$orientation},$base,$info,0,qr/expected irot|expected imir/) if length $transforms;
}
print "$passed metadata validation cases passed\n";
PERL

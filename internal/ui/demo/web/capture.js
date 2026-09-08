function captureFrame(content, cols, rows) {
  if (typeof terminal === 'undefined') {
    terminal = new exports.Terminal({cols:cols,rows:rows,scrollback:0,allowProposedApi:true});
    terminal.unicode.register({version:'crux',wcwidth:characterWidth,charProperties:function(codepoint,previous){
      var width=characterWidth(codepoint),previousWidth=(previous>>1)&3,join=width===0&&previousWidth>0;
      return ((join?previousWidth:width)<<1)|(join?1:0);
    }});
    terminal.unicode.activeVersion='crux';
  }
  terminal.resize(cols,rows);
  terminal.reset();
  var done=false;
  terminal.write('\x1b[?25l\x1b[?7l\x1b[0m\x1b[2J\x1b[H'+content.replace(/\r?\n/g,'\r\n')+'\x1b[0m',function(){done=true;});
  drain();
  if(!done)throw Error('No xterm write acknowledgment');
  var lines=[],cells=[];
  for(var y=0;y<rows;y++) {
    var line=terminal.buffer.active.getLine(terminal.buffer.active.viewportY+y);
    lines.push(line.translateToString(true));
    for(var x=0;x<cols;x++) {
      var c=line.getCell(x);
      if(!c.getWidth())continue;
      cells.push({x:x,y:y,text:c.getChars(),width:c.getWidth(),fg:c.getFgColor(),bg:c.getBgColor(),fgRGB:!!c.isFgRGB(),bgRGB:!!c.isBgRGB(),bold:!!c.isBold(),italic:!!c.isItalic(),dim:!!c.isDim(),inverse:!!c.isInverse(),invisible:!!c.isInvisible(),underline:!!c.isUnderline(),strike:!!c.isStrikethrough()});
    }
  }
  return JSON.stringify({lines:lines,cells:cells});
}

-- major 13

SELECT f.file_format_id, f.name, f.format_type,
       ISNULL(f.field_terminator, ''), ISNULL(f.string_delimiter, ''),
       ISNULL(f.date_format, ''), ISNULL(f.use_type_default, 0),
       ISNULL(f.serde_method, ''), ISNULL(f.row_terminator, ''),
       ISNULL(f.encoding, ''), ISNULL(f.data_compression, ''),
       ISNULL(CAST(0 AS int), 0),
       ISNULL(CAST('' AS nvarchar(10)), '')
FROM   sys.external_file_formats f
-- major 14

SELECT f.file_format_id, f.name, f.format_type,
       ISNULL(f.field_terminator, ''), ISNULL(f.string_delimiter, ''),
       ISNULL(f.date_format, ''), ISNULL(f.use_type_default, 0),
       ISNULL(f.serde_method, ''), ISNULL(f.row_terminator, ''),
       ISNULL(f.encoding, ''), ISNULL(f.data_compression, ''),
       ISNULL(CAST(0 AS int), 0),
       ISNULL(CAST('' AS nvarchar(10)), '')
FROM   sys.external_file_formats f
-- major 15

SELECT f.file_format_id, f.name, f.format_type,
       ISNULL(f.field_terminator, ''), ISNULL(f.string_delimiter, ''),
       ISNULL(f.date_format, ''), ISNULL(f.use_type_default, 0),
       ISNULL(f.serde_method, ''), ISNULL(f.row_terminator, ''),
       ISNULL(f.encoding, ''), ISNULL(f.data_compression, ''),
       ISNULL(f.first_row, 0),
       ISNULL(f.parser_version, '')
FROM   sys.external_file_formats f
-- major 16

SELECT f.file_format_id, f.name, f.format_type,
       ISNULL(f.field_terminator, ''), ISNULL(f.string_delimiter, ''),
       ISNULL(f.date_format, ''), ISNULL(f.use_type_default, 0),
       ISNULL(f.serde_method, ''), ISNULL(f.row_terminator, ''),
       ISNULL(f.encoding, ''), ISNULL(f.data_compression, ''),
       ISNULL(f.first_row, 0),
       ISNULL(f.parser_version, '')
FROM   sys.external_file_formats f
-- major 17

SELECT f.file_format_id, f.name, f.format_type,
       ISNULL(f.field_terminator, ''), ISNULL(f.string_delimiter, ''),
       ISNULL(f.date_format, ''), ISNULL(f.use_type_default, 0),
       ISNULL(f.serde_method, ''), ISNULL(f.row_terminator, ''),
       ISNULL(f.encoding, ''), ISNULL(f.data_compression, ''),
       ISNULL(f.first_row, 0),
       ISNULL(f.parser_version, '')
FROM   sys.external_file_formats f
